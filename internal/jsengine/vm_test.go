package jsengine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEval_1Plus1(t *testing.T) {
	inst, err := New(Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	got, err := inst.Eval(context.Background(), "smoke.js", "1 + 1")
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
	inst, err := New(Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	if _, err := inst.Eval(context.Background(), "setup.js", "globalThis.counter = 0;"); err != nil {
		t.Fatalf("setup eval: %v", err)
	}
	for i := 1; i <= 3; i++ {
		got, err := inst.Eval(context.Background(), "inc.js", "globalThis.counter++; globalThis.counter")
		if err != nil {
			t.Fatalf("inc eval: %v", err)
		}
		want := []string{"", "1", "2", "3"}[i]
		if got != want {
			t.Fatalf("iter %d: want %s, got %q", i, want, got)
		}
	}
}

// TestMemoryLimit_Honoured proves that Limits.MemoryMB is actually wired
// into qjs. A 4 MiB cap must reject a deliberate ~16 MiB allocation. Without
// the wiring this test would silently allocate and pass.
// js-execution.R5
func TestMemoryLimit_Honoured(t *testing.T) {
	inst, err := New(Limits{MemoryMB: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)

	// Allocate a 16 MiB string. With a 4 MiB heap cap, qjs must throw.
	_, err = inst.Eval(context.Background(), "oom.js", `"x".repeat(16 * 1024 * 1024)`)
	if err == nil {
		t.Fatal("expected memory-limit error, got nil — limit not enforced")
	}
	// We don't pin the exact wording (qjs internals), just confirm it failed.
	if !IsOOMError(err) {
		t.Fatalf("IsOOMError must classify qjs OOM, got: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "memory") {
		t.Logf("note: error did not mention memory: %v", err)
	}
}

// js-execution.R4
func TestCallExport_Deadline_IsErrCancelledNotPanic(t *testing.T) {
	vm, err := New(Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := vm.Eval(context.Background(), "m.js", "function spin() { while (true) {} }"); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = vm.CallExport(ctx, "spin") // a panic here fails the test
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}

	// The module is closed now; replacing the VM must not panic.
	vm.Close()
	vm.Close()
}

// js-execution.R4
func TestEval_Deadline_IsErrCancelledNotPanic(t *testing.T) {
	vm, err := New(Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := vm.Eval(ctx, "spin.js", "while (true) {}"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	vm.Close()
}

// js-execution.R9
//
// wazero starts a goroutine per wasm call that reads the runtime context
// later. Many short calls with their own contexts, some cancelled right
// away, give the race detector the chance to see an unsynchronised write.
func TestVM_SequentialCalls_NoContextRace(t *testing.T) {
	vm, err := New(Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer vm.Close()
	if _, err := vm.Eval(context.Background(), "m.js", "function f() { return 1 }"); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	for i := range 200 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if _, err := vm.CallExport(ctx, "f"); err != nil {
			cancel()
			t.Fatalf("call %d: %v", i, err)
		}
		cancel()
	}
}
