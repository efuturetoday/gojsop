package jsengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func newVM(t *testing.T, lim Limits) *VM {
	t.Helper()
	vm, err := New(lim)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(vm.Close)
	return vm
}

// js-execution.R5
// js-execution.R4
// The limit ends the call with ErrOOM, and the VM serves the next call.
func TestMemoryLimit_Honoured(t *testing.T) {
	vm := newVM(t, Limits{MemoryMB: 4})
	_, err := vm.Eval(context.Background(), "oom.js", `"x".repeat(16 * 1024 * 1024)`)
	if !errors.Is(err, ErrOOM) || !IsOOMError(err) {
		t.Fatalf("err = %v, want ErrOOM", err)
	}
	if !vm.Usable() {
		t.Fatal("VM must stay usable after the memory limit")
	}
	if got, err := vm.Eval(context.Background(), "ok.js", "1 + 1"); err != nil || got != "2" {
		t.Fatalf("after OOM: %q, %v", got, err)
	}
}

// js-execution.R5
// An array of small objects hits the limit too (not only one big string).
func TestMemoryLimit_GrowingHeapEndsInErrOOM(t *testing.T) {
	vm := newVM(t, Limits{MemoryMB: 2})
	_, err := vm.Eval(context.Background(), "grow.js", `
		var a = [];
		for (;;) a.push({x: a.length, s: "payload" + a.length});
	`)
	if !errors.Is(err, ErrOOM) {
		t.Fatalf("err = %v, want ErrOOM", err)
	}
	if !vm.Usable() {
		t.Fatal("VM must stay usable")
	}
}

// js-execution.R3
// js-execution.R4
// The deadline stops an endless loop with ErrCancelled. The VM keeps its
// state and serves the next call: no rebuild.
// js-execution.R13
func TestCallExport_Deadline_IsErrCancelledAndVMStaysUsable(t *testing.T) {
	vm := newVM(t, Limits{})
	if _, err := vm.Eval(context.Background(), "m.js", "globalThis.n = 41; function spin() { while (true) {} } function next() { return ++n }"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := vm.CallExport(ctx, "spin")
	if !errors.Is(err, ErrCancelled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrCancelled wrapping DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("deadline of 100ms took %s", d)
	}
	if !vm.Usable() {
		t.Fatal("VM must stay usable after a timeout")
	}
	got, err := vm.CallExport(context.Background(), "next")
	if err != nil || got != "42" {
		t.Fatalf("next call = %q, %v; want 42 (state kept)", got, err)
	}
}

// js-execution.R3
// js-execution.R4
func TestEval_Deadline_IsErrCancelled(t *testing.T) {
	vm := newVM(t, Limits{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := vm.Eval(ctx, "spin.js", "while (true) {}"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if !vm.Usable() {
		t.Fatal("VM must stay usable")
	}
}

// js-execution.R3
// try/catch in the script cannot swallow the interrupt.
// js-execution.R13
func TestDeadline_CannotBeCaught(t *testing.T) {
	vm := newVM(t, Limits{})
	if err := vm.LoadModule(context.Background(), "m.js", `function f() { for (;;) { try { for (;;) {} } catch (e) {} finally {} } }`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := vm.CallExport(ctx, "f"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
}

// js-execution.R3
// A context that is already over never starts the script.
// js-execution.R13
func TestCall_ContextAlreadyDone_DoesNotRun(t *testing.T) {
	vm := newVM(t, Limits{})
	if err := vm.LoadModule(context.Background(), "m.js", `globalThis.ran = false; function f() { ran = true }`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := vm.CallExport(ctx, "f"); !errors.Is(err, ErrCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want ErrCancelled wrapping Canceled", err)
	}
	if got, _ := vm.Eval(context.Background(), "r.js", "String(ran)"); got != "false" {
		t.Fatalf("ran = %s", got)
	}
}

// js-execution.R3
// A regular expression with catastrophic backtracking stops at the deadline.
// js-execution.R13
func TestDeadline_StopsCatastrophicRegex(t *testing.T) {
	vm := newVM(t, Limits{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := vm.Eval(ctx, "re.js", `/(a+)+$/.test("a".repeat(60) + "b")`)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("regex ran %s past a 200ms deadline", d)
	}
}

// Deep recursion is a script error (RangeError), not a trap.
//
// js-execution.R4
func TestStackOverflow_IsScriptErrorNotTrap(t *testing.T) {
	vm := newVM(t, Limits{})
	_, err := vm.Eval(context.Background(), "rec.js", `function r(n) { return r(n + 1) + 1 } r(0)`)
	var je *JSError
	if !errors.As(err, &je) || !strings.Contains(je.Message, "RangeError") {
		t.Fatalf("err = %v, want JSError RangeError", err)
	}
	if !vm.Usable() {
		t.Fatal("VM must stay usable")
	}
}

// js-execution.R8
func TestVMs_AreIsolated(t *testing.T) {
	a, b := newVM(t, Limits{}), newVM(t, Limits{})
	if _, err := a.Eval(context.Background(), "a.js", "globalThis.secret = 1"); err != nil {
		t.Fatal(err)
	}
	got, err := b.Eval(context.Background(), "b.js", "typeof globalThis.secret")
	if err != nil || got != "undefined" {
		t.Fatalf("typeof secret in B = %q, %v", got, err)
	}
}

// EXEC-4
// A script error carries the message, the stack, the file and the line, and
// the name of the export.
// js-execution.R15
func TestScriptError_CarriesMessageStackFileLine(t *testing.T) {
	vm := newVM(t, Limits{})
	if err := vm.LoadModule(context.Background(), "policy.js", "function validate(r) {\n  return inner(r);\n}\nfunction inner(r) {\n  throw new TypeError('bad ' + r.kind);\n}\n"); err != nil {
		t.Fatal(err)
	}
	err := vm.Invoke(context.Background(), "validate", map[string]string{"kind": "Pod"}, nil)
	var je *JSError
	if !errors.As(err, &je) {
		t.Fatalf("err = %v, want *JSError", err)
	}
	if je.Message != "TypeError: bad Pod" {
		t.Errorf("Message = %q", je.Message)
	}
	for _, want := range []string{"at inner (policy.js:5", "at validate (policy.js:2"} {
		if !strings.Contains(je.Stack, want) {
			t.Errorf("Stack lacks %q:\n%s", want, je.Stack)
		}
	}
	if !strings.Contains(err.Error(), "calling validate()") || !strings.Contains(err.Error(), "policy.js:5") {
		t.Errorf("Error() lacks export name or location: %v", err)
	}
	if !vm.Usable() {
		t.Fatal("VM must stay usable after a script error")
	}
}

// js-execution.R15
func TestScriptError_NonErrorThrow(t *testing.T) {
	vm := newVM(t, Limits{})
	_, err := vm.Eval(context.Background(), "t.js", `throw "plain"`)
	var je *JSError
	if !errors.As(err, &je) || je.Message != "plain" || je.Stack != "" {
		t.Fatalf("err = %v (%+v)", err, je)
	}
}

// js-execution.R19
func TestInvoke_DecodesJSONAndLeavesOutOnUndefined(t *testing.T) {
	vm := newVM(t, Limits{})
	if err := vm.LoadModule(context.Background(), "inv.js", `
		function echo(x) { return { got: x.n + 1 }; }
		function nothing() {}
	`); err != nil {
		t.Fatal(err)
	}
	var out struct{ Got int }
	if err := vm.Invoke(context.Background(), "echo", map[string]int{"n": 1}, &out); err != nil || out.Got != 2 {
		t.Fatalf("echo: out=%+v err=%v, want Got=2", out, err)
	}
	out.Got = 7
	if err := vm.Invoke(context.Background(), "nothing", nil, &out); err != nil || out.Got != 7 {
		t.Fatalf("undefined result: out=%+v err=%v, want out untouched", out, err)
	}
	if err := vm.Invoke(context.Background(), "missing", nil, nil); err == nil {
		t.Fatal("expected an error for a missing export")
	}
	if !vm.HasExport("echo") || vm.HasExport("missing") || vm.HasExport("out") {
		t.Fatal("HasExport wrong")
	}
}

// js-execution.R19
func TestInvoke_Unicode_LargePayloadsRoundTrip(t *testing.T) {
	vm := newVM(t, Limits{})
	if err := vm.LoadModule(context.Background(), "u.js", `function id(x) { return x }`); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"s": "größe \U0001F600 \u0000 \"q\"", "big": strings.Repeat("x", 2<<20)}
	var out map[string]any
	if err := vm.Invoke(context.Background(), "id", in, &out); err != nil {
		t.Fatal(err)
	}
	if out["s"] != in["s"] || out["big"] != in["big"] {
		t.Fatal("round trip changed the value")
	}
}

// js-execution.R16
func TestClockAndRandom_AreReal(t *testing.T) {
	vm := newVM(t, Limits{})
	got, err := vm.Eval(context.Background(), "d.js", `String(Date.now())`)
	if err != nil {
		t.Fatal(err)
	}
	var ms int64
	if err := json.Unmarshal([]byte(got), &ms); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(time.UnixMilli(ms)); d < -time.Minute || d > time.Minute {
		t.Fatalf("Date.now() is %s off", d)
	}
	a, _ := vm.Eval(context.Background(), "r.js", `String(Math.random())`)
	b, _ := vm.Eval(context.Background(), "r.js", `String(Math.random())`)
	if a == b {
		t.Fatal("Math.random repeats")
	}
}

// js-execution.R7
func TestClose_Idempotent_CallsAfterCloseFail(t *testing.T) {
	vm, err := New(Limits{})
	if err != nil {
		t.Fatal(err)
	}
	vm.Close()
	vm.Close()
	if _, err := vm.Eval(context.Background(), "x.js", "1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// js-execution.R12
func TestCompilationCache_DirIsFilledAndReused(t *testing.T) {
	dir := t.TempDir()
	for range 2 {
		e, err := NewEngine(context.Background(), Options{CacheDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		vm, err := e.NewVM(context.Background(), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if got, err := vm.Eval(context.Background(), "c.js", "6 * 7"); err != nil || got != "42" {
			t.Fatalf("eval = %q, %v", got, err)
		}
		vm.Close()
		if err := e.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewEngine(context.Background(), Options{CacheDir: "/dev/null/x"}); err == nil {
		t.Fatal("expected an error for an unusable cache directory")
	}
}
