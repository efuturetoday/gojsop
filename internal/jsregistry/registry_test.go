package jsregistry_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsregistry"
)

// js-registry.R5
func TestRegistry_PersistsAcrossLoads(t *testing.T) {
	reg := jsregistry.NewRegistry()
	t.Cleanup(func() { reg.Drop(types.NamespacedName{Name: "h1"}) })

	src := []byte(`globalThis.counter = (globalThis.counter || 0); function config(){return {configVersion:"v1"}}`)
	key := types.NamespacedName{Name: "h1"}
	opts := jsregistry.BuildOptions{Source: src, SourceHash: "abc123"}

	mi, restarted, err := reg.GetOrLoad(context.Background(), key, opts)
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if !restarted {
		t.Fatal("expected restarted=true on first load")
	}
	if got := len(mi.History); got != 0 {
		t.Errorf("History length: got %d, want 0", got)
	}

	if _, err := mi.VM.Eval(context.Background(), "inc.js", "globalThis.counter = 42"); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	mi2, restarted2, err := reg.GetOrLoad(context.Background(), key, opts)
	if err != nil {
		t.Fatalf("GetOrLoad #2: %v", err)
	}
	if restarted2 {
		t.Fatal("expected restarted=false when hash unchanged")
	}
	if mi2 != mi {
		t.Fatal("expected identical ManagedVM pointer")
	}
	got, err := mi2.VM.Eval(context.Background(), "read.js", "globalThis.counter")
	if err != nil {
		t.Fatalf("Eval read: %v", err)
	}
	if got != "42" {
		t.Fatalf("state lost: counter = %q (want 42)", got)
	}
}

// js-registry.R3
// js-sources.R3
func TestRegistry_RestartOnSourceChange(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "h2"}
	t.Cleanup(func() { reg.Drop(key) })

	src1 := []byte(`globalThis.tag = "v1"; function config(){return {}}`)
	src2 := []byte(`globalThis.tag = "v2"; function config(){return {}}`)

	mi, _, err := reg.GetOrLoad(context.Background(), key, jsregistry.BuildOptions{Source: src1, SourceHash: "h1"})
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	got, _ := mi.VM.Eval(context.Background(), "t.js", "globalThis.tag")
	if got != "v1" {
		t.Fatalf("v1 tag: got %q", got)
	}

	mi2, restarted, err := reg.GetOrLoad(context.Background(), key, jsregistry.BuildOptions{Source: src2, SourceHash: "h2"})
	if err != nil {
		t.Fatalf("load v2: %v", err)
	}
	if !restarted {
		t.Fatal("expected restarted=true on source change")
	}
	if got := len(mi2.History); got != 1 {
		t.Errorf("History length: got %d, want 1", got)
	}
	if last := mi2.LastRestart(); last.Reason != jsregistry.ReasonSourceChanged {
		t.Errorf("LastRestart.Reason: got %q", last.Reason)
	}
	if got := mi2.RestartsByReason[jsregistry.ReasonSourceChanged]; got != 1 {
		t.Errorf("RestartsByReason[source-changed]: got %d, want 1", got)
	}
	got2, _ := mi2.VM.Eval(context.Background(), "t.js", "globalThis.tag")
	if got2 != "v2" {
		t.Fatalf("v2 tag: got %q", got2)
	}
}

// TestRegistry_RestartByKey_RebuildsFromCachedSource proves that the registry
// can rescue-rebuild an instance using the cached BuildOptions, without the
// caller passing them again. This is the path the dispatcher takes on
// memory/panic/timeout — it doesn't have the source bytes in hand.
// js-registry.R4
func TestRegistry_RestartByKey_RebuildsFromCachedSource(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "rescue"}
	t.Cleanup(func() { reg.Drop(key) })

	src := []byte(`globalThis.counter = 0; function config(){return {}}`)
	mi, _, err := reg.GetOrLoad(context.Background(), key, jsregistry.BuildOptions{
		Source:     src,
		SourceHash: "h1",
		Limits:     jsengine.Limits{MemoryMB: 8},
	})
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if _, err := mi.VM.Eval(context.Background(), "dirty.js", "globalThis.counter = 99"); err != nil {
		t.Fatalf("dirty eval: %v", err)
	}

	mi2, err := reg.RestartByKey(key, jsregistry.ReasonPanic)
	if err != nil {
		t.Fatalf("RestartByKey: %v", err)
	}
	if mi2 == mi {
		t.Fatal("RestartByKey must return a fresh ManagedVM pointer")
	}
	if last := mi2.LastRestart(); last.Reason != jsregistry.ReasonPanic {
		t.Errorf("LastRestart.Reason: got %q, want %q", last.Reason, jsregistry.ReasonPanic)
	}
	if got := len(mi2.History); got != 1 {
		t.Errorf("History length: got %d, want 1", got)
	}
	if got := mi2.RestartsByReason[jsregistry.ReasonPanic]; got != 1 {
		t.Errorf("RestartsByReason[panic]: got %d, want 1", got)
	}
	if mi2.VM.Limits().MemoryMB != 8 {
		t.Errorf("limits not preserved: got MemoryMB=%d", mi2.VM.Limits().MemoryMB)
	}
	got, err := mi2.VM.Eval(context.Background(), "read.js", "globalThis.counter")
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got != "0" {
		t.Fatalf("globalThis.counter survived rescue: got %q (want 0)", got)
	}
}

// status-conditions.R4
// TestRegistry_RestartHistory_RingAndCounters proves the per-reason counter
// increments correctly across many restarts and that History is capped at
// historyCap (20) with the oldest event evicted on overflow.
// js-registry.R11
func TestRegistry_RestartHistory_RingAndCounters(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "ring"}
	t.Cleanup(func() { reg.Drop(key) })

	src := []byte(`function config(){return {}}`)
	if _, _, err := reg.GetOrLoad(context.Background(), key, jsregistry.BuildOptions{Source: src, SourceHash: "x"}); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	// Three manual restarts → counter == 3, history == 3.
	for i := range 3 {
		if _, err := reg.RestartByKey(key, jsregistry.ReasonManual); err != nil {
			t.Fatalf("restart #%d: %v", i, err)
		}
	}
	mi, _ := reg.Get(key)
	if got := mi.RestartsByReason[jsregistry.ReasonManual]; got != 3 {
		t.Errorf("RestartsByReason[manual] after 3: got %d, want 3", got)
	}
	if got := len(mi.History); got != 3 {
		t.Errorf("History length after 3: got %d, want 3", got)
	}

	// Push another 22 (total 25) — ring should cap at 20, oldest evicted,
	// counter keeps climbing.
	for i := range 22 {
		if _, err := reg.RestartByKey(key, jsregistry.ReasonPanic); err != nil {
			t.Fatalf("restart panic #%d: %v", i, err)
		}
	}
	mi, _ = reg.Get(key)
	if got := len(mi.History); got != 20 {
		t.Errorf("History length capped: got %d, want 20", got)
	}
	if got := mi.RestartsByReason[jsregistry.ReasonManual]; got != 3 {
		t.Errorf("RestartsByReason[manual] preserved: got %d, want 3", got)
	}
	if got := mi.RestartsByReason[jsregistry.ReasonPanic]; got != 22 {
		t.Errorf("RestartsByReason[panic]: got %d, want 22", got)
	}
	// Newest event sits at the tail (oldest-first storage); first three manual
	// events should have been evicted.
	if last := mi.LastRestart(); last.Reason != jsregistry.ReasonPanic {
		t.Errorf("LastRestart.Reason: got %q, want panic", last.Reason)
	}
	for i, ev := range mi.History {
		if ev.Reason != jsregistry.ReasonPanic {
			t.Errorf("History[%d]: got %q, want panic (manual entries should be evicted)", i, ev.Reason)
		}
	}
}

// js-registry.R4
func TestRegistry_RestartByKey_UnknownHook(t *testing.T) {
	reg := jsregistry.NewRegistry()
	if _, err := reg.RestartByKey(types.NamespacedName{Name: "ghost"}, jsregistry.ReasonManual); err == nil {
		t.Fatal("expected error for unknown hook")
	}
}

func TestRegistry_Get(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "g"}
	t.Cleanup(func() { reg.Drop(key) })

	if _, ok := reg.Get(key); ok {
		t.Fatal("Get must return false for unknown key")
	}
	src := []byte(`function config(){return {}}`)
	mi, _, err := reg.GetOrLoad(context.Background(), key, jsregistry.BuildOptions{Source: src, SourceHash: "x"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := reg.Get(key)
	if !ok || got != mi {
		t.Fatal("Get must return the live ManagedVM")
	}
}

func TestRegistry_Drop(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "h3"}

	src := []byte(`function config(){return {}}`)
	if _, _, err := reg.GetOrLoad(context.Background(), key, jsregistry.BuildOptions{Source: src, SourceHash: "x"}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len: got %d, want 1", reg.Len())
	}
	reg.Drop(key)
	if reg.Len() != 0 {
		t.Fatalf("Len after drop: got %d, want 0", reg.Len())
	}
	reg.Drop(key)
}
