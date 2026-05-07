package runtime

import (
	"strings"
	"testing"
)

func TestEval_1Plus1(t *testing.T) {
	inst, err := New(Resources{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	got, err := inst.Eval("smoke.js", "1 + 1")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got != "2" {
		t.Fatalf("expected 2, got %q", got)
	}
}

func TestEval_PersistentState(t *testing.T) {
	// Top-level state must survive between Eval calls — this is the
	// persistent-instance contract we promised users.
	inst, err := New(Resources{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	if _, err := inst.Eval("setup.js", "globalThis.counter = 0;"); err != nil {
		t.Fatalf("setup eval: %v", err)
	}
	for i := 1; i <= 3; i++ {
		got, err := inst.Eval("inc.js", "globalThis.counter++; globalThis.counter")
		if err != nil {
			t.Fatalf("inc eval: %v", err)
		}
		want := []string{"", "1", "2", "3"}[i]
		if got != want {
			t.Fatalf("iter %d: want %s, got %q", i, want, got)
		}
	}
}

// TestMemoryLimit_Honoured proves that Resources.MemoryMB is actually wired
// into qjs. A 4 MiB cap must reject a deliberate ~16 MiB allocation. Without
// the wiring this test would silently allocate and pass.
func TestMemoryLimit_Honoured(t *testing.T) {
	inst, err := New(Resources{MemoryMB: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	// Allocate a 16 MiB string. With a 4 MiB heap cap, qjs must throw.
	_, err = inst.Eval("oom.js", `"x".repeat(16 * 1024 * 1024)`)
	if err == nil {
		t.Fatal("expected memory-limit error, got nil — limit not enforced")
	}
	// We don't pin the exact wording (qjs internals), just confirm it failed.
	if !strings.Contains(strings.ToLower(err.Error()), "memory") &&
		!strings.Contains(strings.ToLower(err.Error()), "out of") {
		t.Logf("note: error did not mention memory: %v", err)
	}
}
