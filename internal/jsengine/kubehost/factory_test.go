package kubehost_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/o-haase/gojsop/internal/jsengine/kubehost"
)

// TestSharedFactory_ForHook_FullSurface asserts ForHook hands out a *KubeHost
// (which Bind installs the full apply/get/list/delete surface) over the
// configured dynamic client and mapper.
// kube-access.R1
func TestSharedFactory_ForHook_FullSurface(t *testing.T) {
	dyn := fake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	f := kubehost.NewSharedFactory(context.Background(), dyn, mapper)

	binder, err := f.ForHook(context.Background(), types.NamespacedName{Namespace: "ns", Name: "h1"}, "")
	if err != nil {
		t.Fatalf("ForHook: %v", err)
	}
	kh, ok := binder.(*kubehost.KubeHost)
	if !ok {
		t.Fatalf("ForHook returned %T, want *KubeHost (full surface)", binder)
	}
	if kh.Dyn == nil {
		t.Error("KubeHost.Dyn is nil — factory did not propagate dynamic client")
	}
	if kh.Mapper == nil {
		t.Error("KubeHost.Mapper is nil — factory did not propagate mapper")
	}
}

// TestSharedFactory_ForAdmission_ReadOnlySurface asserts ForAdmission hands
// out a *ReadOnlyKubeHost — the wrapping that restricts Bind to get/list.
// The underlying client and mapper must match ForHook's so reads see the
// same cluster state.
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
	ro, ok := binder.(*kubehost.ReadOnlyKubeHost)
	if !ok {
		t.Fatalf("ForAdmission returned %T, want *ReadOnlyKubeHost", binder)
	}
	if ro.KubeHost == nil {
		t.Fatal("ReadOnlyKubeHost.KubeHost is nil — factory did not embed inner host")
	}
	if ro.Dyn != dyn || ro.Mapper != mapper {
		t.Error("ForAdmission must share the factory's dynamic client and mapper with ForHook")
	}
}

// TestSharedFactory_PerCallInstances ensures the factory hands out a fresh
// *KubeHost per call — Phase 2's per-SA factory will too, so call sites
// must not assume the binder is shared.
// kube-access.R1
func TestSharedFactory_PerCallInstances(t *testing.T) {
	dyn := fake.NewSimpleDynamicClient(scheme.Scheme)
	mapper := meta.NewDefaultRESTMapper(nil)
	f := kubehost.NewSharedFactory(context.Background(), dyn, mapper)

	a, _ := f.ForHook(context.Background(), types.NamespacedName{Name: "a"}, "")
	b, _ := f.ForHook(context.Background(), types.NamespacedName{Name: "b"}, "")
	if a == b {
		t.Error("ForHook must return a fresh *KubeHost per call so per-call state can't bleed across resources")
	}
}
