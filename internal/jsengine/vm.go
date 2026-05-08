// Package jsengine hosts the JavaScript execution engine that runs each
// JSHook/JSAdmission's user code inside a sandboxed QuickJS instance (via
// fastschema/qjs and Wazero).
//
// The engine is intentionally feature-agnostic: it knows nothing about
// JSHook bindings or JSAdmission requests. Feature packages build their
// own typed wrappers over VM.CallExport.
package jsengine

import (
	"fmt"

	"github.com/fastschema/qjs"
)

// Limits caps what a single VM may consume. Zero fields fall back to the
// defaults in DefaultLimits(); callers are encouraged to pass explicit values
// plumbed from spec.resources on the JSHook/JSAdmission CRDs.
type Limits struct {
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

// DefaultLimits matches the CRD doc defaults (memoryMB:32, timeoutSeconds:30).
func DefaultLimits() Limits {
	return Limits{MemoryMB: 32, TimeoutSeconds: 30}
}

// applyDefaults fills in zero fields from DefaultLimits.
func (l Limits) applyDefaults() Limits {
	d := DefaultLimits()
	if l.MemoryMB <= 0 {
		l.MemoryMB = d.MemoryMB
	}
	if l.TimeoutSeconds <= 0 {
		l.TimeoutSeconds = d.TimeoutSeconds
	}
	return l
}

// maxStackSizeBytes is the QuickJS stack cap. 1 MiB is the qjs README's
// reference value and is plenty for typical hook code without inviting
// pathological recursion.
const maxStackSizeBytes = 1 * 1024 * 1024

// VM is one persistent JS runtime, owned by exactly one JSHook or JSAdmission.
// All Eval/Call must be serialized by the caller (the per-hook FIFO queue
// in the dispatcher does this for us).
type VM struct {
	rt     *qjs.Runtime
	limits Limits
}

// New starts a fresh JS VM with the given Limits. Zero fields fall back to
// DefaultLimits().
func New(lim Limits) (*VM, error) {
	lim = lim.applyDefaults()
	rt, err := qjs.New(qjs.Option{
		MemoryLimit:  int(lim.MemoryMB) * 1024 * 1024,
		MaxStackSize: maxStackSizeBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("qjs.New: %w", err)
	}
	return &VM{rt: rt, limits: lim}, nil
}

// Limits returns the limits this VM was started with.
func (vm *VM) Limits() Limits {
	return vm.limits
}

// Context exposes the underlying qjs context for feature packages that need
// raw qjs bindings (e.g. jsadmission.Handle uses qjs.ToJsValue to stage a
// real JS object instead of a JSON roundtrip).
func (vm *VM) Context() *qjs.Context {
	return vm.rt.Context()
}

// Eval runs source on the underlying runtime and returns the last expression's
// string form. Used for smoke tests and for evaluating the hook's module body.
func (vm *VM) Eval(name, source string) (string, error) {
	ctx := vm.rt.Context()
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
// across subsequent calls — that's the persistent-VM contract.
func (vm *VM) LoadModule(name, source string) error {
	_, err := vm.Eval(name, source)
	return err
}

// CallExport invokes a named global export with the given Go args (converted
// to JS via qjs.ToJsValue) and returns the JSON-encoded return value (or ""
// when the function returned undefined).
//
// CallExport is the feature-agnostic primitive that JSHook and JSAdmission
// build their own typed Handle/ReadConfig wrappers on top of.
func (vm *VM) CallExport(name string, args ...any) (string, error) {
	global := vm.rt.Context().Global()
	fn := global.GetPropertyStr(name)
	defer fn.Free()
	if !fn.IsFunction() {
		return "", fmt.Errorf("module does not export a %s() function", name)
	}
	out, err := global.Invoke(name, args...)
	if err != nil {
		return "", fmt.Errorf("calling %s(): %w", name, err)
	}
	defer out.Free()
	if out.IsUndefined() {
		return "", nil
	}
	s, err := out.JSONStringify()
	if err != nil {
		return "", fmt.Errorf("JSON.stringify %s() result: %w", name, err)
	}
	return s, nil
}

// Close releases the underlying QuickJS runtime. Always call this when the
// owning resource is removed or being restarted.
func (vm *VM) Close() {
	vm.rt.Close()
}
