// Package runtime hosts the JavaScript execution engine that runs each JSHook's
// user code inside a sandboxed QuickJS instance (via fastschema/qjs and Wazero).
//
// The engine is intentionally thin at this stage — it owns one persistent qjs
// runtime per hook so that module-top-level state (caches, counters, expensive
// setups) survives across reconciles, matching the lifecycle agreed in the plan.
package runtime

import (
	"fmt"

	"github.com/fastschema/qjs"
)

// Instance is one persistent JS runtime, owned by exactly one JSHook.
// All Eval/Call must be serialized by the caller (the per-hook FIFO queue
// in the dispatcher does this for us).
type Instance struct {
	rt *qjs.Runtime
}

// New starts a fresh JS instance. It does not yet evaluate user code —
// callers eval the hook source once via Eval, then drive Config()/Handle().
func New() (*Instance, error) {
	rt, err := qjs.New()
	if err != nil {
		return nil, fmt.Errorf("qjs.New: %w", err)
	}
	return &Instance{rt: rt}, nil
}

// Eval runs source on the underlying runtime and returns the last expression's
// string form. Used for smoke tests and for evaluating the hook's module body.
func (i *Instance) Eval(name, source string) (string, error) {
	ctx := i.rt.Context()
	res, err := ctx.Eval(name, qjs.Code(source))
	if err != nil {
		return "", fmt.Errorf("eval %s: %w", name, err)
	}
	defer res.Free()
	return res.String(), nil
}

// Close releases the underlying QuickJS runtime. Always call this when the
// hook is removed or being restarted.
func (i *Instance) Close() {
	i.rt.Close()
}
