package kubehost_test

import (
	"context"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsengine/kubehost"
)

// surfaceOf is what a script would see: the host function names the binder
// registers. Asserting on these rather than on the Go type of the binder is
// what the rules are about — a binder may be composed of several parts.
func surfaceOf(t *testing.T, b jsengine.HostBinder) []string {
	t.Helper()
	h := &jsengine.Host{}
	if err := b.Bind(h); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return h.Names()
}

// ForHook offers the full read+write kube surface.
// kube-access.R1
func TestSharedFactory_ForHook_FullSurface(t *testing.T) {
	dyn := fake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	f := kubehost.NewSharedFactory(context.Background(), dyn, mapper)

	binder, err := f.ForHook(context.Background(), types.NamespacedName{Namespace: "ns", Name: "h1"}, "")
	if err != nil {
		t.Fatalf("ForHook: %v", err)
	}
	got := surfaceOf(t, binder)
	for _, want := range []string{"kube.apply", "kube.delete", "kube.get", "kube.list"} {
		if !slices.Contains(got, want) {
			t.Errorf("ForHook surface is missing %q: %v", want, got)
		}
	}
}

// ForAdmission offers reads only: a policy declares sideEffects: None and the
// apiserver may replay the request, so it must not be able to write.
// kube-access.R2
// js-execution.R6
// jsadmission.R13
func TestSharedFactory_ForAdmission_ReadOnlySurface(t *testing.T) {
	dyn := fake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	f := kubehost.NewSharedFactory(context.Background(), dyn, mapper)

	binder, err := f.ForAdmission(context.Background(), types.NamespacedName{Namespace: "ns", Name: "p1"}, "")
	if err != nil {
		t.Fatalf("ForAdmission: %v", err)
	}
	got := surfaceOf(t, binder)
	for _, want := range []string{"kube.get", "kube.list"} {
		if !slices.Contains(got, want) {
			t.Errorf("ForAdmission surface is missing %q: %v", want, got)
		}
	}
	for _, denied := range []string{"kube.apply", "kube.delete"} {
		if slices.Contains(got, denied) {
			t.Errorf("ForAdmission surface offers %q — a policy must not write: %v", denied, got)
		}
	}
}

// Both surfaces share the factory's client and mapper, so a policy reads the
// same cluster state a hook does.
// kube-access.R2
//
// kube-access.R1
func TestSharedFactory_BothSurfaces_ShareClientAndMapper(t *testing.T) {
	dyn := fake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	f := kubehost.NewSharedFactory(context.Background(), dyn, mapper)

	if f.Dyn != dyn || f.Mapper != mapper {
		t.Fatal("NewSharedFactory did not keep the dynamic client and mapper")
	}
	ro := &kubehost.ReadOnlyKubeHost{KubeHost: &kubehost.KubeHost{Dyn: dyn, Mapper: mapper}}
	if ro.Dyn != dyn || ro.Mapper != mapper {
		t.Error("ReadOnlyKubeHost must reach the inner host's client and mapper")
	}
}

// Every script can say something, on both surfaces: a console line is no
// side effect on the cluster.
//
// js-execution.R17
func TestSharedFactory_BothSurfaces_BindTheConsole(t *testing.T) {
	dyn := fake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	f := kubehost.NewSharedFactory(context.Background(), dyn, mapper)
	key := types.NamespacedName{Namespace: "ns", Name: "x"}

	hook, err := f.ForHook(context.Background(), key, "")
	if err != nil {
		t.Fatalf("ForHook: %v", err)
	}
	pol, err := f.ForAdmission(context.Background(), key, "")
	if err != nil {
		t.Fatalf("ForAdmission: %v", err)
	}
	for name, binder := range map[string]jsengine.HostBinder{"ForHook": hook, "ForAdmission": pol} {
		if got := surfaceOf(t, binder); !slices.Contains(got, "__gojsop_console") {
			t.Errorf("%s surface has no console: %v", name, got)
		}
	}
}

// The factory hands out a fresh binder per call — Phase 2's per-SA factory
// will too, so call sites must not assume the binder is shared.
// kube-access.R1
func TestSharedFactory_PerCallInstances(t *testing.T) {
	dyn := fake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	f := kubehost.NewSharedFactory(context.Background(), dyn, mapper)

	a, _ := f.ForHook(context.Background(), types.NamespacedName{Name: "a"}, "")
	b, _ := f.ForHook(context.Background(), types.NamespacedName{Name: "b"}, "")
	if a == b {
		t.Error("ForHook must return a fresh binder per call so per-call state can't bleed across resources")
	}
}
