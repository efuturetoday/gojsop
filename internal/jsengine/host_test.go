package jsengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func bound(t *testing.T, bind func(*Host)) *VM {
	t.Helper()
	vm := newVM(t, Limits{})
	if err := vm.BindHost(HostBinderFunc(func(h *Host) error { bind(h); return nil })); err != nil {
		t.Fatal(err)
	}
	return vm
}

// js-execution.R11
func TestHost_CallsGoWithJSONAndReturnsJSON(t *testing.T) {
	vm := bound(t, func(h *Host) {
		h.Func("kube.get", func(_ context.Context, arg json.RawMessage) (any, error) {
			var a struct{ Name string }
			if err := json.Unmarshal(arg, &a); err != nil {
				return nil, err
			}
			return map[string]any{"metadata": map[string]any{"name": a.Name}, "n": 1}, nil
		})
		h.Func("kube.none", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
		h.Func("ping", func(_ context.Context, arg json.RawMessage) (any, error) { return string(arg), nil })
	})
	if err := vm.LoadModule(context.Background(), "h.js", `
		function f() { return [kube.get({Name: "x"}).metadata.name, kube.none(), ping(), ping(7)] }
	`); err != nil {
		t.Fatal(err)
	}
	var out []any
	if err := vm.Invoke(context.Background(), "f", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 || out[0] != "x" || out[1] != nil || out[2] != "" || out[3] != "7" {
		t.Fatalf("out = %#v", out)
	}
}

// js-execution.R11
func TestHost_ErrorBecomesCatchableException(t *testing.T) {
	vm := bound(t, func(h *Host) {
		h.Func("kube.get", func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("nope: forbidden") })
	})
	if err := vm.LoadModule(context.Background(), "h.js", `
		function caught() { try { kube.get({}) } catch (e) { return e.message } }
		function uncaught() { kube.get({}) }
	`); err != nil {
		t.Fatal(err)
	}
	var msg string
	if err := vm.Invoke(context.Background(), "caught", nil, &msg); err != nil || msg != "nope: forbidden" {
		t.Fatalf("caught = %q, %v", msg, err)
	}
	err := vm.Invoke(context.Background(), "uncaught", nil, nil)
	var je *JSError
	if !errors.As(err, &je) || !strings.Contains(je.Message, "nope: forbidden") || !strings.Contains(je.Stack, "h.js:3") {
		t.Fatalf("uncaught = %v", err)
	}
	if !vm.Usable() {
		t.Fatal("VM must stay usable")
	}
}

// js-execution.R11
// kube-access.R2
// A function that was not registered is not there: no way around it.
func TestHost_OnlyRegisteredNamesExist(t *testing.T) {
	vm := bound(t, func(h *Host) {
		h.Func("kube.get", func(context.Context, json.RawMessage) (any, error) { return 1, nil })
	})
	got, err := vm.Eval(context.Background(), "p.js", `[typeof kube.get, typeof kube.apply, typeof kube.delete, typeof __gj_host, typeof globalThis.__gj_host].join()`)
	if err != nil || got != "function,undefined,undefined,undefined,undefined" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// js-execution.R11
// The host function gets the context of the running call: the deadline of the
// call ends it (EXEC-2).
func TestHost_FuncSeesCallContext(t *testing.T) {
	vm := bound(t, func(h *Host) {
		h.Func("wait", func(ctx context.Context, _ json.RawMessage) (any, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Second):
				return "late", nil
			}
		})
	})
	if err := vm.LoadModule(context.Background(), "w.js", `function f() { return wait() }`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := vm.Invoke(ctx, "f", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	// The host error is a script exception; the loop-free script ends with it.
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s", d)
	}
	if !vm.Usable() {
		t.Fatal("VM must stay usable")
	}
}

// js-execution.R11
// A script that catches the host error and loops on is still stopped.
func TestHost_DeadlineAfterCaughtHostError(t *testing.T) {
	vm := bound(t, func(h *Host) {
		h.Func("wait", func(ctx context.Context, _ json.RawMessage) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})
	if err := vm.LoadModule(context.Background(), "w.js", `function f() { try { wait() } catch (e) {} for (;;) {} }`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := vm.Invoke(ctx, "f", nil, nil); !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
}

// js-execution.R4
// A panic in a host function is a trap: the VM is broken and says so.
func TestHost_PanicIsTrapAndBreaksVM(t *testing.T) {
	vm := bound(t, func(h *Host) {
		h.Func("boom", func(context.Context, json.RawMessage) (any, error) { panic("host bug") })
	})
	if err := vm.LoadModule(context.Background(), "b.js", `function f() { boom() }`); err != nil {
		t.Fatal(err)
	}
	err := vm.Invoke(context.Background(), "f", nil, nil)
	var trap *TrapError
	if !errors.As(err, &trap) {
		t.Fatalf("err = %v, want *TrapError", err)
	}
	if vm.Usable() {
		t.Fatal("VM must be broken after a trap")
	}
}

func TestBindHost_TwiceFails_BadNameFails(t *testing.T) {
	vm := bound(t, func(h *Host) {})
	if err := vm.BindHost(HostBinderFunc(func(*Host) error { return nil })); err == nil {
		t.Fatal("second BindHost must fail")
	}
	vm2 := newVM(t, Limits{})
	err := vm2.BindHost(HostBinderFunc(func(h *Host) error {
		h.Func("a.b.c", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
		return nil
	}))
	if err == nil {
		t.Fatal("a.b.c must be rejected")
	}
}
