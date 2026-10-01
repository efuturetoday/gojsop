package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsregistry/registrytest"
	"github.com/o-haase/gojsop/internal/jssource"
)

// jshook.R2
func TestReadConfig_PostBuildRejectsMissingExports(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		missing string
	}{
		{"no config", `function handle(c) {}`, "config"},
		{"no handle", `function config() { return {}; }`, "handle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := jsregistry.NewRegistry()
			key := types.NamespacedName{Name: "h"}
			t.Cleanup(func() { reg.Drop(jsregistry.HookKey(key)) })

			_, _, err := registrytest.GetOrLoad(reg, context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{
				Source: []byte(tc.src), SourceHash: "x", PostBuild: readConfig,
			})
			var miss *jsregistry.MissingExportError
			if !errors.As(err, &miss) || miss.Name != tc.missing {
				t.Fatalf("GetOrLoad error = %v, want MissingExportError for %q", err, tc.missing)
			}
			if _, ok := reg.Get(jsregistry.HookKey(key)); ok {
				t.Fatal("a hook that failed to build must not be registered")
			}
		})
	}
}

func newTestReconciler(t *testing.T, backoff jsregistry.Backoff, hooks ...*corev1alpha1.JSHook) (*JSHookReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, h := range hooks {
		b = b.WithObjects(h).WithStatusSubresource(h)
	}
	c := b.Build()
	return &JSHookReconciler{
		Client:   c,
		Scheme:   scheme,
		Loader:   jssource.NewChain(jssource.InlineLoader{}),
		Registry: jsregistry.NewRegistry(),
		Backoff:  backoff,
	}, c
}

func testHook(name, src string, timeoutSeconds int32) *corev1alpha1.JSHook {
	h := &corev1alpha1.JSHook{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec:       corev1alpha1.JSHookSpec{Source: corev1alpha1.JSSource{Inline: src}},
	}
	if timeoutSeconds > 0 {
		h.Spec.Limits = &corev1alpha1.JSLimits{TimeoutSeconds: timeoutSeconds}
	}
	return h
}

func readyCondition(t *testing.T, c client.Client, name string) *metav1.Condition {
	t.Helper()
	var got corev1alpha1.JSHook
	if err := c.Get(t.Context(), types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	return apimeta.FindStatusCondition(got.Status.Conditions, conditions.Ready)
}

// reconcileUntil reconciles name until its Ready condition has the reason.
func reconcileUntil(t *testing.T, r *JSHookReconciler, c client.Client, name, reason string) (ctrl.Result, *metav1.Condition) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		if cond := readyCondition(t, c, name); cond != nil && cond.Reason == reason {
			return res, cond
		}
		if time.Now().After(deadline) {
			t.Fatalf("hook %s never reached reason %s", name, reason)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A top-level while(true){} must not hold the reconcile worker: the reconcile
// of another hook runs and succeeds while the build still hangs.
//
// jshook.R18
// js-registry.R15
// status-conditions.R7
func TestReconcile_HangingBuildDoesNotBlockOtherHook(t *testing.T) {
	good := `function config() { return {}; } function handle() {}`
	r, c := newTestReconciler(t, jsregistry.Backoff{},
		testHook("hang", `while(true){}`, 1), testHook("good", good, 0))
	t.Cleanup(func() {
		r.Registry.Drop(jsregistry.HookKey(types.NamespacedName{Name: "hang"}))
		r.Registry.Drop(jsregistry.HookKey(types.NamespacedName{Name: "good"}))
	})

	start := time.Now()
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "hang"}})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second || res.RequeueAfter != 0 {
		t.Fatalf("reconcile of the hanging hook took %v, requeue %v; want a prompt return without requeue", time.Since(start), res.RequeueAfter)
	}
	if cond := readyCondition(t, c, "hang"); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonBuilding {
		t.Fatalf("hanging hook: want Ready=False/Building, got %+v", cond)
	}

	reconcileUntil(t, r, c, "good", conditions.ReasonReconciled)
	if cond := readyCondition(t, c, "hang"); cond.Reason != conditions.ReasonBuilding {
		t.Fatalf("the hang build ended too early: %+v", cond)
	}
	// The build deadline from the limits ends the hang: BuildFailed.
	_, cond := reconcileUntil(t, r, c, "hang", conditions.ReasonBuildFailed)
	if cond.Status != metav1.ConditionFalse || cond.Message == "" {
		t.Fatalf("want Ready=False with the build error, got %+v", cond)
	}
}

// A broken hook requeues after the backoff from the reconciler's flag values;
// a source change rebuilds at once, without waiting for it.
//
// jshook.R18
// js-registry.R17
// status-conditions.R7
func TestReconcile_BrokenBuild_BacksOffAndSourceChangeRebuildsAtOnce(t *testing.T) {
	backoff := jsregistry.Backoff{Base: time.Hour, Max: 2 * time.Hour}
	r, c := newTestReconciler(t, backoff, testHook("bad", `throw new Error("boom")`, 0))
	key := types.NamespacedName{Name: "bad"}
	t.Cleanup(func() { r.Registry.Drop(jsregistry.HookKey(key)) })

	res, cond := reconcileUntil(t, r, c, "bad", conditions.ReasonBuildFailed)
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("want Ready=False, got %+v", cond)
	}
	// Base is one hour, so the jittered delay lies in [30m, 1h].
	if res.RequeueAfter < 29*time.Minute || res.RequeueAfter > time.Hour {
		t.Fatalf("RequeueAfter = %v, want the backoff derived from the configured base", res.RequeueAfter)
	}
	// Inside the backoff the same source is not rebuilt.
	if res, _ = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); res.RequeueAfter == 0 {
		t.Fatal("reconcile inside the backoff must requeue")
	}
	if cond := readyCondition(t, c, "bad"); cond.Reason != conditions.ReasonBuildFailed {
		t.Fatalf("want BuildFailed inside the backoff, got %+v", cond)
	}

	var h corev1alpha1.JSHook
	if err := c.Get(t.Context(), key, &h); err != nil {
		t.Fatal(err)
	}
	h.Spec.Source.Inline = `function config() { return {}; } function handle() {}`
	if err := c.Update(t.Context(), &h); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if cond := readyCondition(t, c, "bad"); cond.Reason != conditions.ReasonBuilding {
		t.Fatalf("a source change must start a build at once, got %+v", cond)
	}
	reconcileUntil(t, r, c, "bad", conditions.ReasonReconciled)
}
