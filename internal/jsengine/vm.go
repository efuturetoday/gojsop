// Package jsengine hosts the JavaScript execution engine that runs each
// JSHook/JSAdmission's user code inside a sandboxed QuickJS instance (via
// fastschema/qjs and Wazero).
//
// The engine is intentionally feature-agnostic: it knows nothing about
// JSHook bindings or JSAdmission requests. Feature packages build their
// own typed wrappers over VM.CallExport.
package jsengine

import (
	"context"
	"fmt"

	"github.com/fastschema/qjs"
)

// Limits caps what a single VM may consume. Zero fields fall back to the
// defaults in DefaultLimits(); callers are encouraged to pass explicit values
// plumbed from spec.limits on the JSHook/JSAdmission CRDs.
type Limits struct {
	// MemoryMB caps the QuickJS heap in megabytes. Translated to bytes for
	// JS_SetMemoryLimit. Zero means "use default".
	MemoryMB int32
	// TimeoutSeconds bounds a single handle() call. Enforced by passing a
	// context with deadline into Eval/CallExport/LoadModule: wazero exits the
	// in-flight wasm call when the context is cancelled, surfacing as
	// ErrCancelled. The qjs MaxExecutionTime field is a no-op in v0.0.6, so
	// CloseOnContextDone is the actual enforcement mechanism.
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
// DefaultLimits(). The runtime is created with CloseOnContextDone so calls
// can be cancelled by passing a context with deadline into the Eval/Call
// methods below.
func New(lim Limits) (*VM, error) {
	lim = lim.applyDefaults()
	rt, err := qjs.New(qjs.Option{
		MemoryLimit:        int(lim.MemoryMB) * 1024 * 1024,
		MaxStackSize:       maxStackSizeBytes,
		Context:            context.Background(),
		CloseOnContextDone: true,
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
//
// The returned *qjs.Context embeds a context.Context that wazero observes
// for cancellation. Callers MUST NOT mutate that field — use the
// ctx-accepting methods (Eval/CallExport/Invoke) so the per-call swap is
// safe under CallMu.
func (vm *VM) Context() *qjs.Context {
	return vm.rt.Context()
}

// withContext swaps the qjs runtime's embedded Go context for ctx, runs fn,
// and restores the previous context. This is how we plumb a per-call
// deadline into wazero: every wazero invocation reads the embedded context
// (qjs.Context.Context) for cancellation, so swapping it under our CallMu
// gives us per-call cancellation without any qjs-level patching.
//
// The caller is responsible for serialization (in the registry: mi.CallMu).
func (vm *VM) withContext(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	qctx := vm.rt.Context()
	prev := qctx.Context
	qctx.Context = ctx
	defer func() { qctx.Context = prev }()
	return fn()
}

// WithContext runs fn with the qjs runtime's embedded Go context swapped
// for ctx, so any wazero call fn issues (directly, or via qjs primitives
// like Global().Invoke) sees ctx for cancellation. Use this when CallExport
// is too coarse — e.g. jsadmission.Handle decodes a *qjs.Value into a
// Go struct via JsObjectOrMapToGoStruct, which CallExport can't do.
//
// Caller must hold the serialization lock for this VM.
func (vm *VM) WithContext(ctx context.Context, fn func(*qjs.Context) error) error {
	return vm.withContext(ctx, func() error { return fn(vm.rt.Context()) })
}

// WrapEngineErr is the engine's stringly-typed error classifier exported
// for feature wrappers that drive qjs through Context() directly (see
// jsadmission.Handle). Lifts wazero/qjs raw errors into ErrCancelled or
// ErrOOM as appropriate; ctx tells it whether to prefer cancellation.
func WrapEngineErr(ctx context.Context, err error) error {
	return wrapEngineErr(ctx, err)
}

// Eval runs source on the underlying runtime and returns the last expression's
// string form. Used for smoke tests and for evaluating the hook's module body.
//
// ctx is plumbed into wazero via CloseOnContextDone — passing a context with
// deadline gives the call a real timeout (returns ErrCancelled).
func (vm *VM) Eval(ctx context.Context, name, source string) (string, error) {
	var out string
	err := vm.withContext(ctx, func() error {
		res, err := vm.rt.Context().Eval(name, qjs.Code(source))
		if err != nil {
			return fmt.Errorf("eval %s: %w", name, wrapEngineErr(ctx, err))
		}
		defer res.Free()
		out = res.String()
		return nil
	})
	return out, err
}

// LoadModule evaluates the user's hook source on the persistent runtime.
// The source is treated as a script (not an ES module): top-level
// `function config() {}` and `function handle(ctx) {}` declarations become
// globals on globalThis and are callable afterwards.
//
// Module-top-level state (var declarations, globalThis assignments) survives
// across subsequent calls — that's the persistent-VM contract.
func (vm *VM) LoadModule(ctx context.Context, name, source string) error {
	_, err := vm.Eval(ctx, name, source)
	return err
}

// HasExport reports whether the loaded module exposes a callable global
// named name. Used by feature packages to fail builds early with a clear
// "missing required export" error instead of letting the first invocation
// blow up minutes or hours later.
//
// HasExport does not run user JS — it inspects globalThis — so it does not
// take a context.
func (vm *VM) HasExport(name string) bool {
	fn := vm.rt.Context().Global().GetPropertyStr(name)
	defer fn.Free()
	return fn.IsFunction()
}

// CallExport invokes a named global export with the given Go args (converted
// to JS via qjs.ToJsValue) and returns the JSON-encoded return value (or ""
// when the function returned undefined).
//
// CallExport is the feature-agnostic primitive that JSHook and JSAdmission
// build their own typed Handle/ReadConfig wrappers on top of.
func (vm *VM) CallExport(ctx context.Context, name string, args ...any) (string, error) {
	var out string
	err := vm.withContext(ctx, func() error {
		global := vm.rt.Context().Global()
		fn := global.GetPropertyStr(name)
		defer fn.Free()
		if !fn.IsFunction() {
			return fmt.Errorf("module does not export a %s() function", name)
		}
		res, err := global.Invoke(name, args...)
		if err != nil {
			return fmt.Errorf("calling %s(): %w", name, wrapEngineErr(ctx, err))
		}
		defer res.Free()
		if res.IsUndefined() {
			return nil
		}
		s, err := res.JSONStringify()
		if err != nil {
			return fmt.Errorf("JSON.stringify %s() result: %w", name, err)
		}
		out = s
		return nil
	})
	return out, err
}

// Close releases the underlying QuickJS runtime. Always call this when the
// owning resource is removed or being restarted. The caller MUST hold the
// per-VM serialization lock (or otherwise guarantee no in-flight call) —
// closing while a wasm call is mid-flight races inside wazero.
func (vm *VM) Close() {
	vm.rt.Close()
}
