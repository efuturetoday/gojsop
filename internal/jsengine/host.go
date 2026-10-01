package jsengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// HostFunc is a Go function that JavaScript calls. arg is the JSON form of the
// one argument (empty when the script passed none); the result is marshalled to
// JSON for the script (nil becomes null). ctx is the context of the running
// call, so the function ends with the deadline of the call. An error becomes
// an exception in the script, carrying the message.
type HostFunc func(ctx context.Context, arg json.RawMessage) (any, error)

// Host collects the functions a HostBinder offers to one VM.
type Host struct {
	funcs map[string]HostFunc
}

// Func registers fn under name. A dotted name ("kube.get") becomes a
// property of a global object ("kube"); a plain name becomes a global
// function. Registering a name twice replaces it.
func (h *Host) Func(name string, fn HostFunc) {
	if h.funcs == nil {
		h.funcs = map[string]HostFunc{}
	}
	h.funcs[name] = fn
}

// HostBinder offers host-side functions to a fresh VM. Implementations call
// h.Func for every function and do not keep h. Everything crosses through the
// one wasm import env.host_call (js-execution.R11).
type HostBinder interface {
	Bind(h *Host) error
}

// HostBinderFunc adapts a plain function to the HostBinder interface.
type HostBinderFunc func(*Host) error

func (f HostBinderFunc) Bind(h *Host) error { return f(h) }

// BindHost registers the functions of b on this VM and defines them for the
// script. Call it once, before LoadModule, so user code sees them. A name that
// is not registered is not defined: the script sees it as undefined (the
// read-only admission surface relies on it, kube-access.R2).
func (vm *VM) BindHost(b HostBinder) error {
	if b == nil {
		return nil
	}
	if vm.bound {
		return errors.New("jsengine: host already bound")
	}
	h := &Host{}
	if err := b.Bind(h); err != nil {
		return err
	}
	prelude, err := hostPrelude(h.funcs)
	if err != nil {
		return err
	}
	vm.host, vm.bound = h.funcs, true
	if _, err := vm.Eval(context.Background(), "<host>", prelude); err != nil {
		return fmt.Errorf("define host functions: %w", err)
	}
	return nil
}

// hostPrelude is the script that turns the registered names into JavaScript
// functions over the import __gj_host, and then removes the import from the
// global object, so only these closures can reach the host.
func hostPrelude(funcs map[string]HostFunc) (string, error) {
	names := make([]string, 0, len(funcs))
	for n := range funcs {
		names = append(names, n)
	}
	slices.Sort(names)
	var b strings.Builder
	b.WriteString("(function (h) {\n")
	objs := map[string]bool{}
	for _, n := range names {
		q, err := json.Marshal(n)
		if err != nil {
			return "", err
		}
		if obj, prop, ok := strings.Cut(n, "."); ok {
			if strings.Contains(prop, ".") || obj == "" || prop == "" {
				return "", fmt.Errorf("jsengine: host function name %q: want name or object.name", n)
			}
			if !objs[obj] {
				objs[obj] = true
				fmt.Fprintf(&b, "globalThis[%q] = {};\n", obj)
			}
			fmt.Fprintf(&b, "globalThis[%q][%q] = function (a) { return h(%s, a); };\n", obj, prop, q)
		} else {
			fmt.Fprintf(&b, "globalThis[%q] = function (a) { return h(%s, a); };\n", n, q)
		}
	}
	b.WriteString("})(globalThis.__gj_host);\ndelete globalThis.__gj_host;\n")
	return b.String(), nil
}
