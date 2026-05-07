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
	mi, restarted, err := reg.GetOrLoad(key, src, hash)
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
	mi2, restarted2, err := reg.GetOrLoad(key, src, hash)
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

	mi, _, err := reg.GetOrLoad(key, src1, "h1")
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	got, _ := mi.Instance.Eval("t.js", "globalThis.tag")
	if got != "v1" {
		t.Fatalf("v1 tag: got %q", got)
	}

	mi2, restarted, err := reg.GetOrLoad(key, src2, "h2")
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

func TestRegistry_Drop(t *testing.T) {
	reg := NewRegistry()
	key := types.NamespacedName{Name: "h3"}

	src := []byte(`function config(){return {}}`)
	if _, _, err := reg.GetOrLoad(key, src, "x"); err != nil {
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
