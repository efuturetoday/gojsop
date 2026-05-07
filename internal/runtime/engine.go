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

// Resources caps what a single Instance may consume. Zero fields fall back to
// the defaults in DefaultResources(); callers are encouraged to pass explicit
// values plumbed from spec.resources on the JSHook.
type Resources struct {
	// MemoryMB caps the QuickJS heap in megabytes. Translated to bytes for
	// JS_SetMemoryLimit. Zero means "use default".
	MemoryMB int32
	// TimeoutSeconds bounds a single handle() call. Currently advisory —
	// dispatcher logs a warning when exceeded but does not yet interrupt the
	// running call (qjs's MaxExecutionTime is a no-op in v0.0.6, and the
	// per-op CloseOnContextDone path has significant overhead per the qjs
	// docs). Phase 2: real interruption.
	TimeoutSeconds int32
}

// DefaultResources matches the CRD doc defaults (memoryMB:32, timeoutSeconds:30).
func DefaultResources() Resources {
	return Resources{MemoryMB: 32, TimeoutSeconds: 30}
}

// applyDefaults fills in zero fields from DefaultResources.
func (r Resources) applyDefaults() Resources {
	d := DefaultResources()
	if r.MemoryMB <= 0 {
		r.MemoryMB = d.MemoryMB
	}
	if r.TimeoutSeconds <= 0 {
		r.TimeoutSeconds = d.TimeoutSeconds
	}
	return r
}

// maxStackSizeBytes is the QuickJS stack cap. 1 MiB is the qjs README's
// reference value and is plenty for typical hook code without inviting
// pathological recursion.
const maxStackSizeBytes = 1 * 1024 * 1024

// Instance is one persistent JS runtime, owned by exactly one JSHook.
// All Eval/Call must be serialized by the caller (the per-hook FIFO queue
// in the dispatcher does this for us).
type Instance struct {
	rt        *qjs.Runtime
	resources Resources
}

// New starts a fresh JS instance with the given Resources caps. Zero fields
// fall back to DefaultResources(). The instance does not yet evaluate user
// code — callers eval the hook source once via Eval, then drive
// Config()/Handle().
func New(res Resources) (*Instance, error) {
	res = res.applyDefaults()
	rt, err := qjs.New(qjs.Option{
		MemoryLimit:  int(res.MemoryMB) * 1024 * 1024,
		MaxStackSize: maxStackSizeBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("qjs.New: %w", err)
	}
	return &Instance{rt: rt, resources: res}, nil
}

// Resources returns the limits this instance was started with.
func (i *Instance) Resources() Resources {
	return i.resources
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

// Handle invokes the hook's exported `handle(ctx)` function with a
// BindingContext array. The array is shipped as JSON via globalThis and
// awaited inside an IIFE so async hooks work transparently.
//
// Returns the JSON form of whatever handle returned (or "" if it returned
// undefined). Errors from inside JS surface as Go errors.
func (i *Instance) Handle(bindingCtx []BindingContext) (string, error) {
	raw, err := json.Marshal(bindingCtx)
	if err != nil {
		return "", fmt.Errorf("marshal bindingCtx: %w", err)
	}
	if _, err := i.Eval("__handle_setup__", "globalThis.__ctx = "+string(raw)+";"); err != nil {
		return "", fmt.Errorf("seeding ctx: %w", err)
	}
	const call = `(() => {
		if (typeof handle !== "function") {
			throw new Error("hook does not export a handle() function");
		}
		const r = handle(globalThis.__ctx);
		// Promise.resolve().then() returns a Promise; qjs awaits it transparently.
		return r === undefined ? "" : JSON.stringify(r);
	})()`
	out, err := i.Eval("__handle_call__", call)
	if err != nil {
		return "", fmt.Errorf("calling handle(): %w", err)
	}
	return out, nil
}

// TryLoadConfig calls the module's exported `config()` if present and decodes
// the returned object as runtime.Config. Returns (nil, nil) when the module
// does not export config() — JSHook reconciles enforce non-nil at their layer
// (event subscriptions require config), JSAdmission policies don't call this
// path's result at all.
func (i *Instance) TryLoadConfig() (*Config, error) {
	const bridge = `JSON.stringify(typeof config === "function" ? config() : null)`
	raw, err := i.Eval("__config_bridge__", bridge)
	if err != nil {
		return nil, fmt.Errorf("calling config(): %w", err)
	}
	if raw == "null" || raw == "" || raw == "undefined" {
		return nil, nil
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
