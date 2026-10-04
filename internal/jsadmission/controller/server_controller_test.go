package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logr "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/jsadmission"
	"github.com/efuturetoday/gojsop/internal/jsregistry"
	"github.com/efuturetoday/gojsop/internal/jsrun"
	"github.com/efuturetoday/gojsop/internal/jssource"
)

// newServerReconciler returns one "replica": its own registry and its own
// HTTP policy table, sharing only the API server.
func newServerReconciler(t *testing.T, c client.Client) (*JSAdmissionServerReconciler, *jsadmission.Server) {
	t.Helper()
	registry := jsregistry.NewRegistry()
	srv := jsadmission.NewServer(registry, logr.Log)
	return &JSAdmissionServerReconciler{
		Client:  c,
		Loader:  jssource.NewChain(jssource.InlineLoader{}),
		Scripts: registry,
		Server:  srv,
		Backoff: jsrun.Backoff{Base: time.Millisecond, Max: 10 * time.Millisecond},
	}, srv
}

// reconcileUntilRegistered drives r until the policy answers on its handler.
func reconcileUntilRegistered(t *testing.T, r *JSAdmissionServerReconciler, srv *jsadmission.Server, key types.NamespacedName) {
	t.Helper()
	req := ctrl.Request{NamespacedName: key}
	// The first build compiles the QuickJS wasm module, which takes several
	// seconds under -race on a slow runner.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		if admissionStatus(t, srv, key) == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("policy never published locally")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// admissionStatus posts a review to the replica's validate handler and
// returns the HTTP status. 404 means this replica does not know the policy.
func admissionStatus(t *testing.T, srv *jsadmission.Server, key types.NamespacedName) int {
	t.Helper()
	body, err := json.Marshal(admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "AdmissionReview"},
		Request:  &admissionv1.AdmissionRequest{UID: "u1", Operation: admissionv1.Create},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.ValidateHandler().ServeHTTP(w,
		httptest.NewRequest(http.MethodPost, jsadmission.PathFor(key, false), bytes.NewReader(body)))
	return w.Code
}

func admissionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := admissionregv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func admissionPolicy(name string) *corev1alpha1.JSAdmission {
	return &corev1alpha1.JSAdmission{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: corev1alpha1.JSAdmissionSpec{
			Type:   "validating",
			Source: corev1alpha1.JSSource{Inline: "function validate(req) { return { allowed: true }; }"},
		},
	}
}

// Every replica answers every policy, and none of them writes status: the
// webhook Service routes to any ready pod, so a replica that answered 404
// would deny cluster-wide under failurePolicy: Fail.
//
// jsadmission.R20
func TestJSAdmissionServerReconciler_EveryReplicaAnswers(t *testing.T) {
	scheme := admissionScheme(t)
	pol := admissionPolicy("p")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pol).WithStatusSubresource(pol).Build()
	key := types.NamespacedName{Name: "p"}

	a, srvA := newServerReconciler(t, c)
	b, srvB := newServerReconciler(t, c)
	t.Cleanup(func() {
		a.Scripts.Drop(jsrun.AdmissionKey(key))
		b.Scripts.Drop(jsrun.AdmissionKey(key))
	})

	reconcileUntilRegistered(t, a, srvA, key)
	reconcileUntilRegistered(t, b, srvB, key)

	var got corev1alpha1.JSAdmission
	if err := c.Get(t.Context(), key, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Conditions) != 0 || got.Status.ObservedGeneration != 0 {
		t.Fatalf("the server reconciler must not write status, got %+v", got.Status)
	}
}

// A policy on its way out stops being served on this replica at once.
//
// jsadmission.R20
func TestJSAdmissionServerReconciler_DeletionUnregisters(t *testing.T) {
	scheme := admissionScheme(t)
	pol := admissionPolicy("gone")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pol).WithStatusSubresource(pol).Build()
	key := types.NamespacedName{Name: "gone"}

	r, srv := newServerReconciler(t, c)
	t.Cleanup(func() { r.Scripts.Drop(jsrun.AdmissionKey(key)) })
	reconcileUntilRegistered(t, r, srv, key)

	if err := c.Delete(t.Context(), pol); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if code := admissionStatus(t, srv, key); code != http.StatusNotFound {
		t.Fatalf("after deletion the replica answered %d, want 404", code)
	}
	if st, known := r.Scripts.State(jsrun.AdmissionKey(key)); known {
		t.Fatalf("script survived its policy: %+v", st)
	}
}

// The serving controller and its build forwarder must stay out of the
// leader-elected group; nothing else makes a non-leader able to answer.
//
// jsadmission.R20
func TestServerController_IsNotLeaderElected(t *testing.T) {
	opts := serverControllerOptions()
	if opts.NeedLeaderElection == nil || *opts.NeedLeaderElection {
		t.Fatalf("NeedLeaderElection = %v, want a pointer to false", opts.NeedLeaderElection)
	}
	if (everyReplicaRunnable{}).NeedLeaderElection() {
		t.Fatal("the build forwarder must run on every replica")
	}
	// The Registrar relies on the opposite: manager.RunnableFunc carries no
	// NeedLeaderElection, so the manager puts it in the leader-only group.
	// If that ever changes, cmd/main.go has to say so explicitly.
	if _, ok := any(manager.RunnableFunc(func(context.Context) error { return nil })).(manager.LeaderElectionRunnable); ok {
		t.Fatal("manager.RunnableFunc now decides leader election itself; cmd/main.go must be made explicit")
	}
}
