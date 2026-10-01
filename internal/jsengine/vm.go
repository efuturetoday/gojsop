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
	"sync/atomic"
	"time"

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
	// call is the context the qjs runtime currently watches. It is the
	// delegate behind the embedded context of qjs.Context.
	call *callContext
}

// callContext is the one context.Context the qjs runtime carries for its whole
// life. wazero keeps it (as the embedded context of *qjs.Context) and starts a
// goroutine per wasm call that reads Done() at some later time, possibly after
// the call returned. So the embedded field of qjs.Context must never be
// written after New. callContext keeps that field fixed and moves the
// per-call context behind an atomic pointer instead (js-execution.R9).
type callContext struct {
	cur atomic.Pointer[context.Context]
}

func newCallContext() *callContext {
	c := &callContext{}
	c.set(context.Background())
	return c
}

func (c *callContext) set(ctx context.Context) { c.cur.Store(&ctx) }

func (c *callContext) get() context.Context { return *c.cur.Load() }

func (c *callContext) Deadline() (time.Time, bool) { return c.get().Deadline() }
func (c *callContext) Done() <-chan struct{}       { return c.get().Done() }
func (c *callContext) Err() error                  { return c.get().Err() }
func (c *callContext) Value(k any) any             { return c.get().Value(k) }

// New starts a fresh JS VM with the given Limits. Zero fields fall back to
// DefaultLimits(). The runtime is created with CloseOnContextDone so calls
// can be cancelled by passing a context with deadline into the Eval/Call
// methods below.
func New(lim Limits) (*VM, error) {
	lim = lim.applyDefaults()
	cc := newCallContext()
	rt, err := qjs.New(qjs.Option{
		MemoryLimit:        int(lim.MemoryMB) * 1024 * 1024,
		MaxStackSize:       maxStackSizeBytes,
		Context:            cc,
		CloseOnContextDone: true,
	})
	if err != nil {
		return nil, fmt.Errorf("qjs.New: %w", err)
	}
	return &VM{rt: rt, limits: lim, call: cc}, nil
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
// ctx-accepting methods (Eval/CallExport/WithContext) so the per-call context
// is set under CallMu.
func (vm *VM) Context() *qjs.Context {
	return vm.rt.Context()
}

// withContext points the qjs runtime's context at ctx, runs fn, and points it
// back at the background context. This is how we plumb a per-call deadline
// into wazero: every wazero invocation reads the embedded context of
// qjs.Context for cancellation. The embedded value is a fixed callContext that
// delegates to ctx through an atomic pointer, so no field of qjs.Context is
// written while a wazero goroutine of an earlier call may still read it.
//
// The caller is responsible for serialization (in the registry: mi.CallMu).
//
// When ctx ends while fn runs, wazero closes the module and qjs panics on the
// next wasm call instead of returning an error. withContext turns exactly that
// panic into ErrCancelled, so the call is classified as cancelled and not as a
// panic. The module stays closed: the VM is dead after such a call and the
// caller must rebuild it. A closed-module panic while ctx is still live, and
// every other panic, propagate unchanged.
//
// js-execution.R4
func (vm *VM) withContext(ctx context.Context, fn func() error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	vm.call.set(ctx)
	defer vm.call.set(context.Background())
	defer func() {
		if p := recover(); p != nil {
			if ctx.Err() != nil && closedModulePanic(p) {
				err = fmt.Errorf("%w: %v", ErrCancelled, p)
				return
			}
			panic(p)
		}
	}()
	return fn()
}

// WithContext runs fn with the qjs runtime's Go context pointed at ctx, so any wazero call fn issues (directly, or via qjs primitives
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
//
// Closing a VM whose module wazero already closed (a cancelled call, see
// withContext) is allowed and does not panic: qjs panics in QJS_Free then,
// and that one panic is swallowed. The module is gone either way; anything
// else qjs panics about still propagates.
func (vm *VM) Close() {
	defer func() {
		if p := recover(); p != nil && !closedModulePanic(p) {
			panic(p)
		}
	}()
	vm.rt.Close()
}
