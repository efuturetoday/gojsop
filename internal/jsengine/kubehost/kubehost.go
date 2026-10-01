package kubehost

import (
	"context"
	"fmt"

	"github.com/fastschema/qjs"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// FieldManager is the server-side-apply field manager used by kube.apply.
const FieldManager = "gojsop"

// KubeHost binds globalThis.kube.{apply,get,list,delete} into a JS runtime.
// One KubeHost is shared across all hook instances; all calls run in the
// goroutine of the per-hook FIFO worker that's invoking handle(), so this
// type does not need additional synchronisation.
// Block: kube-access R7
type KubeHost struct {
	// Ctx is the parent context handed to every dynamic-client call. Letting
	// it be cancelled (e.g. when the manager stops) propagates cancellation
	// into in-flight K8s API calls. nil falls back to context.Background.
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

// Bind installs the full kube.{apply,get,list,delete} surface on globalThis.
// Implements HostBinder. Used for JSHook VMs.
// Block: kube-access R3
func (h *KubeHost) Bind(ctx *qjs.Context) error {
	if h.Dyn == nil || h.Mapper == nil {
		return fmt.Errorf("KubeHost: Dyn and Mapper must be set")
	}
	kube := ctx.NewObject()
	kube.SetPropertyStr("apply", ctx.Function(h.apply))
	kube.SetPropertyStr("get", ctx.Function(h.get))
	kube.SetPropertyStr("list", ctx.Function(h.list))
	kube.SetPropertyStr("delete", ctx.Function(h.del))
	ctx.Global().SetPropertyStr("kube", kube)
	return nil
}

// ReadOnlyKubeHost is a HostBinder that exposes only kube.get and kube.list
// to JS. kube.apply and kube.delete are not bound at all, so JS sees them
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

// Bind installs only the read-only subset on globalThis. Implements HostBinder.
// Block: kube-access R2
func (h *ReadOnlyKubeHost) Bind(ctx *qjs.Context) error {
	if h.KubeHost == nil || h.Dyn == nil || h.Mapper == nil {
		return fmt.Errorf("ReadOnlyKubeHost: embedded KubeHost must be set")
	}
	kube := ctx.NewObject()
	kube.SetPropertyStr("get", ctx.Function(h.get))
	kube.SetPropertyStr("list", ctx.Function(h.list))
	ctx.Global().SetPropertyStr("kube", kube)
	return nil
}

// Block: kube-access R5
func (h *KubeHost) callCtx() context.Context {
	if h.Ctx != nil {
		return h.Ctx
	}
	return context.Background()
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

// apply performs a Get-then-Create-or-MergePatch of the JS object:
// kube.apply({apiVersion, kind, metadata:{name,namespace}, ...}). Returns the
// persisted object. The body is free-form K8s JSON, so we keep it as a generic
// map and feed it directly into unstructured.Unstructured.
// Block: kube-access R4
func (h *KubeHost) apply(t *qjs.This) (*qjs.Value, error) {
	args := t.Args()
	if len(args) == 0 {
		return nil, fmt.Errorf("kube.apply: missing object argument")
	}
	m, err := qjs.JsObjectOrMapToGoMap[map[string]any](args[0])
	if err != nil {
		return nil, fmt.Errorf("kube.apply: convert arg: %w", err)
	}
	obj := &unstructured.Unstructured{Object: m}
	if obj.GetAPIVersion() == "" || obj.GetKind() == "" || obj.GetName() == "" {
		return nil, fmt.Errorf("kube.apply: object requires apiVersion, kind and metadata.name")
	}
	rc, err := h.resourceFor(obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace())
	if err != nil {
		return nil, err
	}
	// Get-then-create-or-merge: portable across real clusters and fake test
	// clients. Phase 2 may switch to server-side apply once we depend on
	// modern API servers exclusively.
	existing, getErr := rc.Get(h.callCtx(), obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) {
		res, err := rc.Create(h.callCtx(), obj, metav1.CreateOptions{FieldManager: FieldManager})
		if err != nil {
			return nil, fmt.Errorf("kube.apply: create: %w", err)
		}
		return objToJS(t.Context(), res)
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
	res, err := rc.Patch(h.callCtx(), obj.GetName(), types.MergePatchType, data, metav1.PatchOptions{
		FieldManager: FieldManager,
	})
	if err != nil {
		return nil, fmt.Errorf("kube.apply: patch: %w", err)
	}
	return objToJS(t.Context(), res)
}

// get returns the named resource, or null if it does not exist.
func (h *KubeHost) get(t *qjs.This) (*qjs.Value, error) {
	ref, err := argAsStruct[kubeRef](t, "kube.get")
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
	res, err := rc.Get(h.callCtx(), ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return t.Context().NewNull(), nil
		}
		return nil, fmt.Errorf("kube.get: %w", err)
	}
	return objToJS(t.Context(), res)
}

// list returns an array of objects matching the selector.
func (h *KubeHost) list(t *qjs.This) (*qjs.Value, error) {
	spec, err := argAsStruct[kubeListSpec](t, "kube.list")
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
	res, err := rc.List(h.callCtx(), metav1.ListOptions{
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
	v, err := qjs.ToJsValue(t.Context(), items)
	if err != nil {
		return nil, fmt.Errorf("kube.list: convert result: %w", err)
	}
	return v, nil
}

// del removes the named resource. Missing-resource is treated as success.
func (h *KubeHost) del(t *qjs.This) (*qjs.Value, error) {
	ref, err := argAsStruct[kubeRef](t, "kube.delete")
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
	if err := rc.Delete(h.callCtx(), ref.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kube.delete: %w", err)
	}
	return t.Context().NewBool(true), nil
}

// argAsStruct decodes args[0] directly into a typed Go struct using qjs's
// canonical helper. Field mapping honours `json:"..."` tags.
func argAsStruct[T any](t *qjs.This, fnName string) (T, error) {
	var zero T
	args := t.Args()
	if len(args) == 0 {
		return zero, fmt.Errorf("%s: missing object argument", fnName)
	}
	v, err := qjs.JsObjectOrMapToGoStruct[T](args[0])
	if err != nil {
		return zero, fmt.Errorf("%s: convert arg: %w", fnName, err)
	}
	return v, nil
}

// objToJS hands an unstructured object back to JS as a real JS object via
// qjs.ToJsValue. Ownership of the returned *Value transfers to JS.
func objToJS(ctx *qjs.Context, obj *unstructured.Unstructured) (*qjs.Value, error) {
	v, err := qjs.ToJsValue(ctx, obj.Object)
	if err != nil {
		return nil, fmt.Errorf("convert object to JS: %w", err)
	}
	return v, nil
}
