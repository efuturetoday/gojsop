// Package runtime hosts the JavaScript execution engine that runs each JSHook's
// user code inside a sandboxed QuickJS instance (via fastschema/qjs and Wazero).
//
// The engine is intentionally thin at this stage — it owns one persistent qjs
// runtime per hook so that module-top-level state (caches, counters, expensive
// setups) survives across reconciles, matching the lifecycle agreed in the plan.
package runtime

import (
	"encoding/json"
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

// LoadModule evaluates the user's hook source on the persistent runtime.
// The source is treated as a script (not an ES module): top-level
// `function config() {}` and `function handle(ctx) {}` declarations become
// globals on globalThis and are callable afterwards.
//
// Module-top-level state (var declarations, globalThis assignments) survives
// across subsequent Handle/LoadConfig calls — that's the persistent-instance
// contract.
func (i *Instance) LoadModule(name, source string) error {
	_, err := i.Eval(name, source)
	return err
}

// LoadConfig calls the hook's exported `config()` function and decodes the
// returned object as our runtime.Config. The hook MUST export config() —
// missing or non-function returns an error (we can't subscribe blindly).
func (i *Instance) LoadConfig() (*Config, error) {
	const bridge = `JSON.stringify(typeof config === "function" ? config() : null)`
	raw, err := i.Eval("__config_bridge__", bridge)
	if err != nil {
		return nil, fmt.Errorf("calling config(): %w", err)
	}
	if raw == "null" || raw == "" || raw == "undefined" {
		return nil, fmt.Errorf("hook does not export a config() function")
	}
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("config() returned non-JSON: %w (raw=%s)", err, raw)
	}
	return &cfg, nil
}

// Close releases the underlying QuickJS runtime. Always call this when the
// hook is removed or being restarted.
func (i *Instance) Close() {
	i.rt.Close()
}
