package runtime

import (
	"context"
	"encoding/json"
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

// apply performs server-side-apply of the JS object: kube.apply({apiVersion,
// kind, metadata:{name,namespace}, ...}). Returns the persisted object.
func (h *KubeHost) apply(t *qjs.This) (*qjs.Value, error) {
	raw, err := stringifyArg(t, "kube.apply")
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON([]byte(raw)); err != nil {
		return nil, fmt.Errorf("kube.apply: parse object: %w (raw=%q)", err, raw)
	}
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
	data, err := json.Marshal(obj.Object)
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

type kubeRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
}

// get returns the named resource, or null if it does not exist.
func (h *KubeHost) get(t *qjs.This) (*qjs.Value, error) {
	raw, err := stringifyArg(t, "kube.get")
	if err != nil {
		return nil, err
	}
	var ref kubeRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		return nil, fmt.Errorf("kube.get: parse argument: %w", err)
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

type kubeListSpec struct {
	APIVersion     string `json:"apiVersion"`
	Kind           string `json:"kind"`
	Namespace      string `json:"namespace"`
	LabelSelector  string `json:"labelSelector"`
	FieldSelector  string `json:"fieldSelector"`
}

// list returns an array of objects matching the selector.
func (h *KubeHost) list(t *qjs.This) (*qjs.Value, error) {
	raw, err := stringifyArg(t, "kube.list")
	if err != nil {
		return nil, err
	}
	var spec kubeListSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, fmt.Errorf("kube.list: parse argument: %w", err)
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
	data, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("kube.list: marshal: %w", err)
	}
	return t.Context().ParseJSON(string(data)), nil
}

// del removes the named resource. Missing-resource is treated as success.
func (h *KubeHost) del(t *qjs.This) (*qjs.Value, error) {
	raw, err := stringifyArg(t, "kube.delete")
	if err != nil {
		return nil, err
	}
	var ref kubeRef
	if err := json.Unmarshal([]byte(raw), &ref); err != nil {
		return nil, fmt.Errorf("kube.delete: parse argument: %w", err)
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

// stringifyArg pulls args[0] off `t` and JSONStringifies it. We've seen a rare
// production case where JSONStringify returns "" for what looks like a normal
// object literal — likely a qjs handle-lifetime quirk under GC pressure. As a
// fallback we route the conversion through the JS JSON global, which uses the
// argument by reference instead of going through the cloned handle.
func stringifyArg(t *qjs.This, fnName string) (string, error) {
	args := t.Args()
	if len(args) == 0 {
		return "", fmt.Errorf("%s: missing object argument", fnName)
	}
	raw, err := args[0].JSONStringify()
	if err != nil {
		return "", fmt.Errorf("%s: stringify arg: %w", fnName, err)
	}
	if raw != "" {
		return raw, nil
	}
	// Fallback: invoke JSON.stringify(arg) via the JS global. This avoids the
	// cloned-handle path that occasionally returns "".
	ctx := t.Context()
	jsonGlobal := ctx.Global().GetPropertyStr("JSON")
	if jsonGlobal == nil || jsonGlobal.IsUndefined() {
		return "", fmt.Errorf("%s: stringify arg returned empty and no JSON global available", fnName)
	}
	res, err := jsonGlobal.Invoke("stringify", args[0])
	if err != nil {
		return "", fmt.Errorf("%s: stringify fallback: %w", fnName, err)
	}
	defer res.Free()
	out := res.String()
	if out == "" || out == "undefined" {
		return "", fmt.Errorf("%s: argument serialised to undefined (typeof=%s)", fnName, jsTypeOf(args[0]))
	}
	return out, nil
}

func jsTypeOf(v *qjs.Value) string {
	switch {
	case v == nil || v.IsUndefined():
		return "undefined"
	case v.IsNull():
		return "null"
	case v.IsString():
		return "string"
	case v.IsNumber():
		return "number"
	case v.IsBool():
		return "boolean"
	case v.IsArray():
		return "array"
	case v.IsObject():
		return "object"
	default:
		return "unknown"
	}
}

func objToJS(ctx *qjs.Context, obj *unstructured.Unstructured) (*qjs.Value, error) {
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("marshal object: %w", err)
	}
	return ctx.ParseJSON(string(data)), nil
}
