package kubehost

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/o-haase/gojsop/internal/jsengine"
)

// Factory mints a per-resource HostBinder. One Factory exists per process;
// implementations decide whether each call returns a shared client (today)
// or a per-ServiceAccount client minted via TokenRequest (Phase 2).
//
// ForHook returns a binder with the full read+write kube.* surface; ForAdmission
// returns a binder restricted to kube.get/kube.list (admission policies declare
// sideEffects: None and the apiserver may retry).
//
// The sa argument is the ServiceAccount the resource asked to run as. Today's
// SharedFactory ignores it; Phase 2 threads it into TokenRequest.
type Factory interface {
	ForHook(ctx context.Context, key types.NamespacedName, sa string) (jsengine.HostBinder, error)
	ForAdmission(ctx context.Context, key types.NamespacedName, sa string) (jsengine.HostBinder, error)
}

// SharedFactory hands every caller a binder over the same process-wide
// dynamic client and RESTMapper. It exists to preserve today's behaviour
// while letting reconcilers drop the singleton from their fields.
type SharedFactory struct {
	Ctx    context.Context
	Dyn    dynamic.Interface
	Mapper meta.RESTMapper
}

// NewSharedFactory builds a SharedFactory; ctx is propagated into every
// dynamic-client call (manager shutdown cancels in-flight K8s API requests).
func NewSharedFactory(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper) *SharedFactory {
	return &SharedFactory{Ctx: ctx, Dyn: dyn, Mapper: mapper}
}

// ForHook returns a *KubeHost over the shared client. key and sa are
// accepted for the Factory contract but ignored: SharedFactory has no
// per-hook scoping to apply.
func (f *SharedFactory) ForHook(_ context.Context, _ types.NamespacedName, _ string) (jsengine.HostBinder, error) {
	return &KubeHost{Ctx: f.Ctx, Dyn: f.Dyn, Mapper: f.Mapper}, nil
}

// ForAdmission returns a *ReadOnlyKubeHost wrapping a fresh *KubeHost over
// the shared client. The wrapping is what restricts the bound surface; the
// underlying client and mapper are identical to ForHook's.
func (f *SharedFactory) ForAdmission(_ context.Context, _ types.NamespacedName, _ string) (jsengine.HostBinder, error) {
	return &ReadOnlyKubeHost{KubeHost: &KubeHost{Ctx: f.Ctx, Dyn: f.Dyn, Mapper: f.Mapper}}, nil
}
