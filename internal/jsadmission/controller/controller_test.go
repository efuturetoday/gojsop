package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logr "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsadmission"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsrun"
	"github.com/o-haase/gojsop/internal/jssource"
)

// jsadmission.R17
func TestReconcile_RegistrarSyncFailure_ShowsReadyFalse(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := admissionregv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pol := &corev1alpha1.JSAdmission{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Generation: 1},
		Spec: corev1alpha1.JSAdmissionSpec{
			Type:   "validating",
			Source: corev1alpha1.JSSource{Inline: "function validate(req) { return { allowed: true }; }"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(pol).WithStatusSubresource(pol).Build()

	reg := jsadmission.NewRegistrar(c, admissionregv1.ServiceReference{Name: "svc", Namespace: "ns"},
		func(context.Context) ([]byte, error) { return nil, errors.New("ca unreadable") }, logr.Log)
	reg.Debounce = time.Millisecond
	reg.RetryDelay = time.Hour
	ctx := t.Context()
	reg.Start(ctx)

	registry := jsregistry.NewRegistry()
	srv := &JSAdmissionServerReconciler{
		Client:  c,
		Loader:  jssource.NewChain(jssource.InlineLoader{}),
		Scripts: registry,
		Server:  jsadmission.NewServer(registry, logr.Log),
	}
	r := &JSAdmissionReconciler{
		Client:    c,
		Scheme:    scheme,
		Loader:    jssource.NewChain(jssource.InlineLoader{}),
		Scripts:   registry,
		Registrar: reg,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}
	// The first reconciles find the VM Building; go on until it is built.
	// The first build compiles the QuickJS wasm module, which takes several
	// seconds under -race on a slow CI runner.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := srv.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		st, _ := registry.State(jsrun.AdmissionKey(req.NamespacedName))
		built := st.Phase == jsrun.PhaseReady
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		if built { // this reconcile found the VM Ready and published the policy
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("VM never built")
		}
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for reg.SyncError() == nil {
		if time.Now().After(deadline) {
			t.Fatal("registrar never failed")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	var got corev1alpha1.JSAdmission
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, conditions.Ready)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonWebhookSyncFailed {
		t.Fatalf("want Ready=False/%s, got %+v", conditions.ReasonWebhookSyncFailed, cond)
	}
	if cond.ObservedGeneration != 1 {
		t.Fatalf("observedGeneration: %d", cond.ObservedGeneration)
	}
}

// While the VM is not ready the policy is Ready=False with reason Building or
// BuildFailed; no reconcile waits for the build.
//
// jsadmission.R18
// jsadmission.R22
// js-registry.R15
// status-conditions.R7
func TestReconcile_BuildStates_ShowBuildingThenBuildFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pol := &corev1alpha1.JSAdmission{
		ObjectMeta: metav1.ObjectMeta{Name: "hang", Generation: 1},
		Spec: corev1alpha1.JSAdmissionSpec{
			Type:   "validating",
			Source: corev1alpha1.JSSource{Inline: "while(true){}"},
			Limits: &corev1alpha1.JSLimits{TimeoutSeconds: 1},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pol).WithStatusSubresource(pol).Build()
	registry := jsregistry.NewRegistry()
	srv := &JSAdmissionServerReconciler{
		Client:  c,
		Loader:  jssource.NewChain(jssource.InlineLoader{}),
		Scripts: registry,
		Server:  jsadmission.NewServer(registry, logr.Log),
		Backoff: jsrun.Backoff{Base: time.Hour, Max: time.Hour},
	}
	r := &JSAdmissionReconciler{
		Client:  c,
		Scheme:  scheme,
		Loader:  jssource.NewChain(jssource.InlineLoader{}),
		Scripts: registry,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "hang"}}
	t.Cleanup(func() { registry.Drop(jsrun.AdmissionKey(req.NamespacedName)) })
	ready := func() *metav1.Condition {
		var got corev1alpha1.JSAdmission
		if err := c.Get(t.Context(), req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		return apimeta.FindStatusCondition(got.Status.Conditions, conditions.Ready)
	}

	start := time.Now()
	if _, err := srv.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second || res.RequeueAfter != 0 {
		t.Fatalf("reconcile took %v, requeue %v; want a prompt return without requeue", time.Since(start), res.RequeueAfter)
	}
	if cond := ready(); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonBuilding {
		t.Fatalf("want Ready=False/Building, got %+v", cond)
	}

	deadline := time.Now().Add(20 * time.Second)
	for ready().Reason != conditions.ReasonBuildFailed {
		if time.Now().After(deadline) {
			t.Fatalf("never BuildFailed: %+v", ready())
		}
		if _, err := srv.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		var err error
		if res, err = r.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if res.RequeueAfter < 29*time.Minute || res.RequeueAfter > time.Hour {
		t.Fatalf("RequeueAfter = %v, want the backoff derived from the configured base", res.RequeueAfter)
	}
}

// The server reports a timeout after the smaller of spec.timeoutSeconds and
// spec.limits.timeoutSeconds, defaults applied.
//
// jsadmission.R11
func TestCallTimeout_SmallerOfWebhookAndLimits(t *testing.T) {
	cases := []struct {
		name    string
		webhook int32
		lim     jsrun.Limits
		want    time.Duration
	}{
		{"defaults", 0, jsrun.Limits{}, 5 * time.Second},
		{"webhook smaller", 10, jsrun.Limits{TimeoutSeconds: 20}, 10 * time.Second},
		{"limits smaller", 10, jsrun.Limits{TimeoutSeconds: 2}, 2 * time.Second},
		{"limit default above webhook max", 30, jsrun.Limits{}, 30 * time.Second},
	}
	for _, tc := range cases {
		if got := callTimeout(tc.webhook, tc.lim); got != tc.want {
			t.Errorf("%s: callTimeout(%d, %+v) = %v, want %v", tc.name, tc.webhook, tc.lim, got, tc.want)
		}
	}
}

// kube-access.R8
func TestSetupWithManager_RequiresKubeHost(t *testing.T) {
	registry := jsregistry.NewRegistry()
	r := &JSAdmissionServerReconciler{Scripts: registry, Server: jsadmission.NewServer(registry, logr.Log)}
	if err := r.SetupWithManager(nil); err == nil {
		t.Fatal("SetupWithManager without a kubehost.Factory must fail")
	}
}
