package kubehost

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jslog"
)

// Factory mints a per-resource HostBinder. One Factory exists per process;
// every binder reaches the cluster as the ServiceAccount it was minted for.
//
// ForHook returns a binder with the full read+write kube.* surface; ForAdmission
// returns a binder restricted to kube.get/kube.list (admission policies declare
// sideEffects: None and the apiserver may retry).
//
// The sa argument is the ServiceAccount the resource runs as; empty means the
// operator's own client.
// kube-access.R1
type Factory interface {
	ForHook(ctx context.Context, key types.NamespacedName, sa string) (jsengine.HostBinder, error)
	ForAdmission(ctx context.Context, key types.NamespacedName, sa string) (jsengine.HostBinder, error)
}

// SharedFactory hands every caller a binder over one process-wide RESTMapper
// and a dynamic client that acts as the caller's ServiceAccount.
type SharedFactory struct {
	Ctx    context.Context
	Dyn    dynamic.Interface
	Mapper meta.RESTMapper
	// As returns a client that acts as the ServiceAccount sa. When set, a
	// binder for a non-empty sa uses it instead of Dyn, so a script reaches
	// the cluster with the rights of its own ServiceAccount.
	// kube-access.R12
	As func(sa string) (dynamic.Interface, error)
}

// clientFor is the client a binder for sa uses.
func (f *SharedFactory) clientFor(sa string) (dynamic.Interface, error) {
	if sa == "" || f.As == nil {
		return f.Dyn, nil
	}
	return f.As(sa)
}

// NewSharedFactory builds a SharedFactory; ctx is propagated into every
// dynamic-client call (manager shutdown cancels in-flight K8s API requests).
func NewSharedFactory(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper) *SharedFactory {
	return &SharedFactory{Ctx: ctx, Dyn: dyn, Mapper: mapper}
}

// ForHook returns a *KubeHost acting as sa, plus the script console.
func (f *SharedFactory) ForHook(_ context.Context, _ types.NamespacedName, sa string) (jsengine.HostBinder, error) {
	dyn, err := f.clientFor(sa)
	if err != nil {
		return nil, err
	}
	return jsengine.Binders(
		&KubeHost{Ctx: f.Ctx, Dyn: dyn, Mapper: f.Mapper},
		jslog.Binder{},
		HookEvents{},
	), nil
}

// ForAdmission returns a *ReadOnlyKubeHost wrapping a fresh *KubeHost over
// the shared client, plus the script console. The wrapping is what restricts
// the bound surface; the underlying client and mapper are identical to
// ForHook's. The console is bound on both surfaces: it writes nowhere but
// into the sink of the call, so it is no side effect on the cluster.
// kube-access.R2
// jsadmission.R13
func (f *SharedFactory) ForAdmission(_ context.Context, _ types.NamespacedName, sa string) (jsengine.HostBinder, error) {
	dyn, err := f.clientFor(sa)
	if err != nil {
		return nil, err
	}
	return jsengine.Binders(
		&ReadOnlyKubeHost{KubeHost: &KubeHost{Ctx: f.Ctx, Dyn: dyn, Mapper: f.Mapper}},
		jslog.Binder{},
	), nil
}
