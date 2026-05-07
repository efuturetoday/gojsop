package runtime

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestRegistry_PersistsAcrossLoads(t *testing.T) {
	reg := NewRegistry()
	t.Cleanup(func() { reg.Drop(types.NamespacedName{Name: "h1"}) })

	src := []byte(`globalThis.counter = (globalThis.counter || 0); function config(){return {configVersion:"v1"}}`)
	hash := "abc123"
	key := types.NamespacedName{Name: "h1"}

	// First load: starts a new instance.
	mi, restarted, err := reg.GetOrLoad(key, src, hash, Resources{})
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if !restarted {
		t.Fatal("expected restarted=true on first load")
	}
	if mi.RestartCount != 0 {
		t.Errorf("RestartCount: got %d, want 0", mi.RestartCount)
	}

	// Mutate global state on the instance.
	if _, err := mi.Instance.Eval("inc.js", "globalThis.counter = 42"); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	// Second load with same hash: must return the same instance, state intact.
	mi2, restarted2, err := reg.GetOrLoad(key, src, hash, Resources{})
	if err != nil {
		t.Fatalf("GetOrLoad #2: %v", err)
	}
	if restarted2 {
		t.Fatal("expected restarted=false when hash unchanged")
	}
	if mi2 != mi {
		t.Fatal("expected identical ManagedInstance pointer")
	}
	got, err := mi2.Instance.Eval("read.js", "globalThis.counter")
	if err != nil {
		t.Fatalf("Eval read: %v", err)
	}
	if got != "42" {
		t.Fatalf("state lost: counter = %q (want 42)", got)
	}
}

func TestRegistry_RestartOnSourceChange(t *testing.T) {
	reg := NewRegistry()
	key := types.NamespacedName{Name: "h2"}
	t.Cleanup(func() { reg.Drop(key) })

	src1 := []byte(`globalThis.tag = "v1"; function config(){return {}}`)
	src2 := []byte(`globalThis.tag = "v2"; function config(){return {}}`)

	mi, _, err := reg.GetOrLoad(key, src1, "h1", Resources{})
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	got, _ := mi.Instance.Eval("t.js", "globalThis.tag")
	if got != "v1" {
		t.Fatalf("v1 tag: got %q", got)
	}

	mi2, restarted, err := reg.GetOrLoad(key, src2, "h2", Resources{})
	if err != nil {
		t.Fatalf("load v2: %v", err)
	}
	if !restarted {
		t.Fatal("expected restarted=true on source change")
	}
	if mi2.RestartCount != 1 {
		t.Errorf("RestartCount: got %d, want 1", mi2.RestartCount)
	}
	if mi2.LastReason != ReasonSourceChanged {
		t.Errorf("LastReason: got %q", mi2.LastReason)
	}
	got2, _ := mi2.Instance.Eval("t.js", "globalThis.tag")
	if got2 != "v2" {
		t.Fatalf("v2 tag: got %q", got2)
	}
}

// TestRegistry_RestartByKey_RebuildsFromCachedSource proves that the registry
// can rescue-rebuild an instance using the cached source/resources, without
// the caller passing them again. This is the path the dispatcher takes on
// memory/panic/timeout — it doesn't have the source bytes in hand.
func TestRegistry_RestartByKey_RebuildsFromCachedSource(t *testing.T) {
	reg := NewRegistry()
	key := types.NamespacedName{Name: "rescue"}
	t.Cleanup(func() { reg.Drop(key) })

	src := []byte(`globalThis.counter = 0; function config(){return {}}`)
	mi, _, err := reg.GetOrLoad(key, src, "h1", Resources{MemoryMB: 8})
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	// Dirty the global state so we can prove the rebuild wiped it.
	if _, err := mi.Instance.Eval("dirty.js", "globalThis.counter = 99"); err != nil {
		t.Fatalf("dirty eval: %v", err)
	}

	mi2, err := reg.RestartByKey(key, ReasonPanic)
	if err != nil {
		t.Fatalf("RestartByKey: %v", err)
	}
	if mi2 == mi {
		t.Fatal("RestartByKey must return a fresh ManagedInstance pointer")
	}
	if mi2.LastReason != ReasonPanic {
		t.Errorf("LastReason: got %q, want %q", mi2.LastReason, ReasonPanic)
	}
	if mi2.RestartCount != 1 {
		t.Errorf("RestartCount: got %d, want 1", mi2.RestartCount)
	}
	if mi2.Instance.Resources().MemoryMB != 8 {
		t.Errorf("resources not preserved: got MemoryMB=%d", mi2.Instance.Resources().MemoryMB)
	}
	got, err := mi2.Instance.Eval("read.js", "globalThis.counter")
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got != "0" {
		t.Fatalf("globalThis.counter survived rescue: got %q (want 0)", got)
	}
}

func TestRegistry_RestartByKey_UnknownHook(t *testing.T) {
	reg := NewRegistry()
	if _, err := reg.RestartByKey(types.NamespacedName{Name: "ghost"}, ReasonManual); err == nil {
		t.Fatal("expected error for unknown hook")
	}
}

func TestRegistry_Get(t *testing.T) {
	reg := NewRegistry()
	key := types.NamespacedName{Name: "g"}
	t.Cleanup(func() { reg.Drop(key) })

	if _, ok := reg.Get(key); ok {
		t.Fatal("Get must return false for unknown key")
	}
	src := []byte(`function config(){return {}}`)
	mi, _, err := reg.GetOrLoad(key, src, "x", Resources{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := reg.Get(key)
	if !ok || got != mi {
		t.Fatal("Get must return the live ManagedInstance")
	}
}

func TestIsOOMError(t *testing.T) {
	if IsOOMError(nil) {
		t.Fatal("nil error must not be classified as OOM")
	}
	if IsOOMError(errOf("some other failure")) {
		t.Fatal("unrelated error must not be classified as OOM")
	}
	// The exact wording qjs v0.0.6 emits — see TestMemoryLimit_Honoured.
	if !IsOOMError(errOf("eval oom.js: InternalError: out of memory\n    at <eval> (oom.js:1:23)")) {
		t.Fatal("qjs OOM message must be classified")
	}
	// Case-insensitive — defensive against future qjs releases.
	if !IsOOMError(errOf("Out Of Memory")) {
		t.Fatal("matching must be case-insensitive")
	}
}

type stringErr string

func (e stringErr) Error() string { return string(e) }
func errOf(s string) error        { return stringErr(s) }

func TestRegistry_Drop(t *testing.T) {
	reg := NewRegistry()
	key := types.NamespacedName{Name: "h3"}

	src := []byte(`function config(){return {}}`)
	if _, _, err := reg.GetOrLoad(key, src, "x", Resources{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len: got %d, want 1", reg.Len())
	}
	reg.Drop(key)
	if reg.Len() != 0 {
		t.Fatalf("Len after drop: got %d, want 0", reg.Len())
	}
	// idempotent
	reg.Drop(key)
}
