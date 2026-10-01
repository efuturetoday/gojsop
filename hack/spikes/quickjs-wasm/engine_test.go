package quickjswasm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var testLimits = Limits{MemoryBytes: 32 << 20, StackBytes: 512 << 10}

func newTestEngine(tb testing.TB) *Engine {
	tb.Helper()
	ctx := context.Background()
	e, err := NewEngine(ctx, EngineOptions{MaxMemoryPages: 1024})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = e.Close(ctx) })
	return e
}

func newTestVM(tb testing.TB, e *Engine) *VM {
	tb.Helper()
	vm, err := e.NewVM(context.Background(), testLimits)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = vm.Close(context.Background()) })
	return vm
}

func TestValidate_SmallAndLarge(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	req := admissionRequest(t)
	for name, src := range map[string]string{"small": smallPolicy, "large": largePolicy(t)} {
		vm := newTestVM(t, e)
		if err := vm.Eval(ctx, name, src); err != nil {
			t.Fatalf("%s: eval: %v", name, err)
		}
		out, err := vm.Call(ctx, "validate", req)
		if err != nil {
			t.Fatalf("%s: call: %v", name, err)
		}
		if string(out) != `{"allowed":true}` {
			t.Fatalf("%s: got %s", name, out)
		}
	}
}

func TestException_HasMessageAndStack(t *testing.T) {
	e := newTestEngine(t)
	vm := newTestVM(t, e)
	ctx := context.Background()
	if err := vm.Eval(ctx, "policy.js", "function validate(r) {\n  return r.no.such;\n}"); err != nil {
		t.Fatal(err)
	}
	_, err := vm.Call(ctx, "validate", []byte(`{}`))
	var jsErr *JSError
	if !errors.As(err, &jsErr) {
		t.Fatalf("want JSError, got %v", err)
	}
	if !strings.Contains(jsErr.Text, "TypeError") || !strings.Contains(jsErr.Text, "policy.js:2") {
		t.Fatalf("no message or line: %q", jsErr.Text)
	}
}

// The interrupt handler stops an endless loop at the deadline and leaves the
// VM alive with its state: no rebuild after a timeout.
func TestInterrupt_StopsLoopAndVMSurvives(t *testing.T) {
	e := newTestEngine(t)
	vm := newTestVM(t, e)
	if err := vm.Eval(context.Background(), "s", `
		var calls = 0;
		function spin() { calls++; for (;;) {} }
		function count() { return calls; }
	`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := vm.Call(ctx, "spin", []byte(`null`))
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("want ErrInterrupted, got %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("interrupt took %v", d)
	}
	out, err := vm.Call(context.Background(), "count", []byte(`null`))
	if err != nil || string(out) != "1" {
		t.Fatalf("VM did not survive: %s %v", out, err)
	}
}

// A script cannot catch the interrupt.
func TestInterrupt_NotCatchable(t *testing.T) {
	e := newTestEngine(t)
	vm := newTestVM(t, e)
	if err := vm.Eval(context.Background(), "s", `
		function spin() { for (;;) { try { for (;;) {} } catch (e) {} } }
	`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := vm.Call(ctx, "spin", []byte(`null`)); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("want ErrInterrupted, got %v", err)
	}
}

// The QuickJS memory limit ends the call with ErrOOM, and the VM stays usable.
func TestMemoryLimit_OOMAndVMSurvives(t *testing.T) {
	e := newTestEngine(t)
	vm := newTestVM(t, e)
	ctx := context.Background()
	if err := vm.Eval(ctx, "s", `
		function hog() { const a = []; for (;;) a.push("x".repeat(1024) + a.length); }
		function ok() { return 42; }
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Call(ctx, "hog", []byte(`null`)); !errors.Is(err, ErrOOM) {
		t.Fatalf("want ErrOOM, got %v", err)
	}
	out, err := vm.Call(ctx, "ok", []byte(`null`))
	if err != nil || string(out) != "42" {
		t.Fatalf("VM did not survive OOM: %s %v", out, err)
	}
}

// Bytecode from one VM runs in another VM of the same engine build.
func TestBytecode_RunsInFreshVM(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	bc, err := newTestVM(t, e).Compile(ctx, "large", largePolicy(t), false)
	if err != nil {
		t.Fatal(err)
	}
	vm := newTestVM(t, e)
	if err := vm.EvalBytecode(ctx, bc); err != nil {
		t.Fatal(err)
	}
	out, err := vm.Call(ctx, "validate", admissionRequest(t))
	if err != nil || string(out) != `{"allowed":true}` {
		t.Fatalf("got %s %v", out, err)
	}
	t.Logf("source %d B, bytecode %d B", len(largePolicy(t)), len(bc))
}

// A VM restored from a snapshot has the state of the snapshot, and VMs
// restored from one snapshot do not share state.
func TestSnapshot_RestoresStateAndIsolates(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	base := newTestVM(t, e)
	if err := base.Eval(ctx, "s", largePolicy(t)+`
		var n = 0;
		function inc() { return ++n; }
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := base.Call(ctx, "inc", []byte(`null`)); err != nil {
		t.Fatal(err)
	}
	snap := base.Snapshot()
	t.Logf("snapshot %d KiB of %d pages", len(snap.Data)/1024, snap.Pages)

	a, err := e.NewVMFromSnapshot(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(ctx) }()
	b, err := e.NewVMFromSnapshot(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close(ctx) }()
	for _, want := range []string{"2", "3"} {
		if out, err := a.Call(ctx, "inc", []byte(`null`)); err != nil || string(out) != want {
			t.Fatalf("a: got %s %v, want %s", out, err, want)
		}
	}
	if out, err := b.Call(ctx, "inc", []byte(`null`)); err != nil || string(out) != "2" {
		t.Fatalf("b shares state with a: got %s %v", out, err)
	}
	if out, err := a.Call(ctx, "validate", admissionRequest(t)); err != nil || string(out) != `{"allowed":true}` {
		t.Fatalf("validate after restore: %s %v", out, err)
	}
	// The interrupt still works in a restored VM.
	if err := a.Eval(ctx, "spin", `function spin() { for (;;) {} }`); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := a.Call(cctx, "spin", []byte(`null`)); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("want ErrInterrupted, got %v", err)
	}
}
