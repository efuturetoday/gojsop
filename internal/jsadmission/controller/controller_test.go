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

	r := &JSAdmissionReconciler{
		Client:    c,
		Scheme:    scheme,
		Loader:    jssource.NewChain(jssource.InlineLoader{}),
		Registry:  jsregistry.NewRegistry(),
		Registrar: reg,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "p"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
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
