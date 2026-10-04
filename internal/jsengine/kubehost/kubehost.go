package kubehost

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/o-haase/gojsop/internal/jsengine"
)

// FieldManager is the field manager kube.apply sends with its create and merge
// patch. kube.apply is not server-side apply (kube-access.R4).
const FieldManager = "gojsop"

// KubeHost binds globalThis.kube.{apply,get,list,delete} into a JS runtime.
// One KubeHost serves every call of a script, and calls run in parallel. Its
// fields are set once and only read, so it needs no locking.
// kube-access.R7
type KubeHost struct {
	// Ctx is a parent context for every call: when it ends (e.g. the manager
	// stops), in-flight K8s API calls end too. Every call runs under the
	// context of the JS call, so spec.limits.timeoutSeconds bounds it
	// (kube-access.R5). nil means no parent.
	Ctx    context.Context
	Dyn    dynamic.Interface
	Mapper meta.RESTMapper
}

// kubeRef identifies a single resource — used by kube.get and kube.delete.
type kubeRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

// kubeListSpec is the argument shape for kube.list.
type kubeListSpec struct {
	APIVersion    string `json:"apiVersion"`
	Kind          string `json:"kind"`
	Namespace     string `json:"namespace"`
	LabelSelector string `json:"labelSelector"`
	FieldSelector string `json:"fieldSelector"`
}

// Bind registers the full kube.{apply,get,list,delete} surface. Implements
// jsengine.HostBinder. Used for JSHook VMs.
// kube-access.R3
func (h *KubeHost) Bind(host *jsengine.Host) error {
	if h.Dyn == nil || h.Mapper == nil {
		return fmt.Errorf("KubeHost: Dyn and Mapper must be set")
	}
	host.Func("kube.apply", h.apply)
	host.Func("kube.get", h.get)
	host.Func("kube.list", h.list)
	host.Func("kube.delete", h.del)
	return nil
}

// ReadOnlyKubeHost is a HostBinder that exposes only kube.get and kube.list
// to JS. kube.apply and kube.delete are not registered at all, so JS sees them
// as undefined. Intended for JSAdmission VMs where the CRD declares
// sideEffects: None and any cluster write from the request path is a
// contract violation.
//
// The embedded *KubeHost is the source of the dynamic client and mapper;
// this type is intentionally a thin wrapper so the read-only and full
// binders share zero state divergence — they're literally the same client,
// with a different surface bound into JS.
type ReadOnlyKubeHost struct {
	*KubeHost
}

// Bind registers only the read-only subset. Implements jsengine.HostBinder.
// kube-access.R2
func (h *ReadOnlyKubeHost) Bind(host *jsengine.Host) error {
	if h.KubeHost == nil || h.Dyn == nil || h.Mapper == nil {
		return fmt.Errorf("ReadOnlyKubeHost: embedded KubeHost must be set")
	}
	host.Func("kube.get", h.get)
	host.Func("kube.list", h.list)
	return nil
}

// callCtx is the context of one kube.* call: the context of the running JS
// call (its deadline is spec.limits.timeoutSeconds), which also ends when the
// parent h.Ctx ends (manager shutdown). The returned stop func must be called.
// kube-access.R5
func (h *KubeHost) callCtx(call context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(call)
	if h.Ctx == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(h.Ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// resourceFor maps an apiVersion/kind pair to a dynamic.ResourceInterface
// scoped to namespace if the resource is namespaced.
func (h *KubeHost) resourceFor(apiVersion, kind, namespace string) (dynamic.ResourceInterface, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, fmt.Errorf("apiVersion %q: %w", apiVersion, err)
	}
	rm, err := h.Mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
	if err != nil {
		return nil, fmt.Errorf("RESTMapping %s/%s: %w", apiVersion, kind, err)
	}
	if rm.Scope.Name() == meta.RESTScopeNameNamespace {
		return h.Dyn.Resource(rm.Resource).Namespace(namespace), nil
	}
	return h.Dyn.Resource(rm.Resource), nil
}

// decodeArg reads the one JSON argument of a kube.* call into T.
func decodeArg[T any](arg json.RawMessage, fnName string) (T, error) {
	var v T
	if len(arg) == 0 {
		return v, fmt.Errorf("%s: missing object argument", fnName)
	}
	if err := json.Unmarshal(arg, &v); err != nil {
		return v, fmt.Errorf("%s: convert arg: %w", fnName, err)
	}
	return v, nil
}

// apply performs a Get-then-Create-or-MergePatch of the JS object:
// kube.apply({apiVersion, kind, metadata:{name,namespace}, ...}). Returns the
// persisted object. The body is free-form K8s JSON, so we keep it as a generic
// map and feed it directly into unstructured.Unstructured.
// kube-access.R4
func (h *KubeHost) apply(call context.Context, arg json.RawMessage) (any, error) {
	m, err := decodeArg[map[string]any](arg, "kube.apply")
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{Object: m}
	if obj.GetAPIVersion() == "" || obj.GetKind() == "" || obj.GetName() == "" {
		return nil, fmt.Errorf("kube.apply: object requires apiVersion, kind and metadata.name")
	}
	rc, err := h.resourceFor(obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	ctx, done := h.callCtx(call)
	defer done()
	// Get-then-create-or-merge: portable across real clusters and fake test
	// clients. Phase 2 may switch to server-side apply once we depend on
	// modern API servers exclusively.
	existing, getErr := rc.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) {
		res, err := rc.Create(ctx, obj, metav1.CreateOptions{FieldManager: FieldManager})
		if err != nil {
			return nil, fmt.Errorf("kube.apply: create: %w", err)
		}
		return res.Object, nil
	}
	if getErr != nil {
		return nil, fmt.Errorf("kube.apply: get: %w", getErr)
	}
	// Preserve the existing resourceVersion so the merge is conflict-safe.
	obj.SetResourceVersion(existing.GetResourceVersion())
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("kube.apply: marshal: %w", err)
	}
	res, err := rc.Patch(ctx, obj.GetName(), types.MergePatchType, data, metav1.PatchOptions{
		FieldManager: FieldManager,
	})
	if err != nil {
		return nil, fmt.Errorf("kube.apply: patch: %w", err)
	}
	return res.Object, nil
}

// get returns the named resource, or null if it does not exist.
func (h *KubeHost) get(call context.Context, arg json.RawMessage) (any, error) {
	ref, err := decodeArg[kubeRef](arg, "kube.get")
	if err != nil {
		return nil, err
	}
	if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return nil, fmt.Errorf("kube.get: apiVersion, kind and name are required")
	}
	rc, err := h.resourceFor(ref.APIVersion, ref.Kind, ref.Namespace)
	if err != nil {
		return nil, err
	}
	ctx, done := h.callCtx(call)
	defer done()
	res, err := rc.Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("kube.get: %w", err)
	}
	return res.Object, nil
}

// list returns an array of objects matching the selector.
func (h *KubeHost) list(call context.Context, arg json.RawMessage) (any, error) {
	spec, err := decodeArg[kubeListSpec](arg, "kube.list")
	if err != nil {
		return nil, err
	}
	if spec.APIVersion == "" || spec.Kind == "" {
		return nil, fmt.Errorf("kube.list: apiVersion and kind are required")
	}
	rc, err := h.resourceFor(spec.APIVersion, spec.Kind, spec.Namespace)
	if err != nil {
		return nil, err
	}
	ctx, done := h.callCtx(call)
	defer done()
	res, err := rc.List(ctx, metav1.ListOptions{
		LabelSelector: spec.LabelSelector,
		FieldSelector: spec.FieldSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("kube.list: %w", err)
	}
	items := make([]map[string]any, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, res.Items[i].Object)
	}
	return items, nil
}

// del removes the named resource. Missing-resource is treated as success.
func (h *KubeHost) del(call context.Context, arg json.RawMessage) (any, error) {
	ref, err := decodeArg[kubeRef](arg, "kube.delete")
	if err != nil {
		return nil, err
	}
	if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return nil, fmt.Errorf("kube.delete: apiVersion, kind and name are required")
	}
	rc, err := h.resourceFor(ref.APIVersion, ref.Kind, ref.Namespace)
	if err != nil {
		return nil, err
	}
	ctx, done := h.callCtx(call)
	defer done()
	if err := rc.Delete(ctx, ref.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kube.delete: %w", err)
	}
	return true, nil
}
