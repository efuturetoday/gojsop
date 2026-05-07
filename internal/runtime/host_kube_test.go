package runtime

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

// testMapper is a minimal RESTMapper covering the GVKs the host_kube tests
// exercise. It avoids pulling in a real discovery client.
func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "", Version: "v1"}})
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	return m
}

func newKubeHost(t *testing.T, seed ...runtime.Object) *KubeHost {
	t.Helper()
	scheme := runtime.NewScheme()
	dyn := fake.NewSimpleDynamicClient(scheme, seed...)
	return &KubeHost{Ctx: context.Background(), Dyn: dyn, Mapper: testMapper()}
}

// runHook spins up a runtime, binds kube.*, evaluates source and returns the
// instance for further inspection.
func runHook(t *testing.T, h *KubeHost, source string) *Instance {
	t.Helper()
	inst, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)
	if err := inst.BindHost(h); err != nil {
		t.Fatalf("BindHost: %v", err)
	}
	if source != "" {
		if _, err := inst.Eval("hook.js", source); err != nil {
			t.Fatalf("Eval: %v", err)
		}
	}
	return inst
}

func TestKubeHost_ApplyCreatesAndUpdates(t *testing.T) {
	h := newKubeHost(t)
	const src = `
		globalThis.runApply = function(data) {
			return kube.apply({
				apiVersion: "v1",
				kind: "ConfigMap",
				metadata: { name: "demo", namespace: "default" },
				data: data,
			});
		};
	`
	inst := runHook(t, h, src)

	// First apply — creates the resource.
	out, err := inst.Eval("call1.js", `JSON.stringify(runApply({key: "v1"}))`)
	if err != nil {
		t.Fatalf("apply create: %v", err)
	}
	if out == "" || out == "null" {
		t.Fatalf("apply returned empty result: %q", out)
	}

	got, err := h.Dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).
		Namespace("default").Get(context.Background(), "demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("post-apply Get: %v", err)
	}
	data, _, _ := unstructured.NestedStringMap(got.Object, "data")
	if data["key"] != "v1" {
		t.Fatalf("expected data.key=v1, got %v", data)
	}

	// Second apply — updates the resource.
	if _, err := inst.Eval("call2.js", `runApply({key: "v2"})`); err != nil {
		t.Fatalf("apply update: %v", err)
	}
	got, err = h.Dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).
		Namespace("default").Get(context.Background(), "demo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("post-update Get: %v", err)
	}
	data, _, _ = unstructured.NestedStringMap(got.Object, "data")
	if data["key"] != "v2" {
		t.Fatalf("expected data.key=v2 after update, got %v", data)
	}
}

func TestKubeHost_GetReturnsNullForMissing(t *testing.T) {
	h := newKubeHost(t)
	inst := runHook(t, h, ``)
	out, err := inst.Eval("get.js", `JSON.stringify(kube.get({
		apiVersion: "v1", kind: "ConfigMap", namespace: "default", name: "missing"
	}))`)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out != "null" {
		t.Fatalf("expected null for missing resource, got %q", out)
	}
}

func TestKubeHost_ListReturnsItems(t *testing.T) {
	cm1 := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata":   map[string]any{"name": "a", "namespace": "default"},
	}}
	cm2 := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata":   map[string]any{"name": "b", "namespace": "default"},
	}}
	h := newKubeHost(t, cm1, cm2)

	inst := runHook(t, h, ``)
	out, err := inst.Eval("list.js", `(() => {
		const items = kube.list({apiVersion: "v1", kind: "ConfigMap", namespace: "default"});
		return items.map(i => i.metadata.name).sort().join(",");
	})()`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out != "a,b" {
		t.Fatalf("expected list a,b, got %q", out)
	}
}

func TestKubeHost_DeleteRemovesResource(t *testing.T) {
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata":   map[string]any{"name": "doomed", "namespace": "default"},
	}}
	h := newKubeHost(t, cm)
	inst := runHook(t, h, ``)
	if _, err := inst.Eval("del.js", `kube.delete({
		apiVersion: "v1", kind: "ConfigMap", namespace: "default", name: "doomed"
	})`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := h.Dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).
		Namespace("default").Get(context.Background(), "doomed", metav1.GetOptions{})
	if err == nil {
		t.Fatal("expected NotFound after delete, got nil")
	}
}
