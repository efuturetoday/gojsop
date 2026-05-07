package runtime

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
type KubeHost struct {
	// Ctx is the parent context handed to every dynamic-client call. Letting
	// it be cancelled (e.g. when the manager stops) propagates cancellation
	// into in-flight K8s API calls. nil falls back to context.Background.
	Ctx    context.Context
	Dyn    dynamic.Interface
	Mapper meta.RESTMapper
}

// Bind installs the kube.* functions on globalThis. Implements HostBinder.
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
// persisted object.
func (h *KubeHost) apply(t *qjs.This) (*qjs.Value, error) {
	m, err := argAsMap(t, "kube.apply")
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
	m, err := argAsMap(t, "kube.get")
	if err != nil {
		return nil, err
	}
	apiVersion, err := stringField(m, "apiVersion", "kube.get", true)
	if err != nil {
		return nil, err
	}
	kind, err := stringField(m, "kind", "kube.get", true)
	if err != nil {
		return nil, err
	}
	name, err := stringField(m, "name", "kube.get", true)
	if err != nil {
		return nil, err
	}
	namespace, _ := stringField(m, "namespace", "kube.get", false)

	rc, err := h.resourceFor(apiVersion, kind, namespace)
	if err != nil {
		return nil, err
	}
	res, err := rc.Get(h.callCtx(), name, metav1.GetOptions{})
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
	m, err := argAsMap(t, "kube.list")
	if err != nil {
		return nil, err
	}
	apiVersion, err := stringField(m, "apiVersion", "kube.list", true)
	if err != nil {
		return nil, err
	}
	kind, err := stringField(m, "kind", "kube.list", true)
	if err != nil {
		return nil, err
	}
	namespace, _ := stringField(m, "namespace", "kube.list", false)
	labelSelector, _ := stringField(m, "labelSelector", "kube.list", false)
	fieldSelector, _ := stringField(m, "fieldSelector", "kube.list", false)

	rc, err := h.resourceFor(apiVersion, kind, namespace)
	if err != nil {
		return nil, err
	}
	res, err := rc.List(h.callCtx(), metav1.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: fieldSelector,
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
	m, err := argAsMap(t, "kube.delete")
	if err != nil {
		return nil, err
	}
	apiVersion, err := stringField(m, "apiVersion", "kube.delete", true)
	if err != nil {
		return nil, err
	}
	kind, err := stringField(m, "kind", "kube.delete", true)
	if err != nil {
		return nil, err
	}
	name, err := stringField(m, "name", "kube.delete", true)
	if err != nil {
		return nil, err
	}
	namespace, _ := stringField(m, "namespace", "kube.delete", false)

	rc, err := h.resourceFor(apiVersion, kind, namespace)
	if err != nil {
		return nil, err
	}
	if err := rc.Delete(h.callCtx(), name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kube.delete: %w", err)
	}
	return t.Context().NewBool(true), nil
}

// argAsMap pulls args[0] off `t` and converts it directly to a Go map via the
// canonical qjs helper — no JSON detour, no handle leaks.
func argAsMap(t *qjs.This, fnName string) (map[string]any, error) {
	args := t.Args()
	if len(args) == 0 {
		return nil, fmt.Errorf("%s: missing object argument", fnName)
	}
	m, err := qjs.JsObjectOrMapToGoMap[map[string]any](args[0])
	if err != nil {
		return nil, fmt.Errorf("%s: convert arg: %w", fnName, err)
	}
	return m, nil
}

// stringField reads a string field from a map with a clear error when required.
func stringField(m map[string]any, key, fnName string, required bool) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		if required {
			return "", fmt.Errorf("%s: %s is required", fnName, key)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s: %s must be a string, got %T", fnName, key, v)
	}
	return s, nil
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
