package jsengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Status codes of glue.c.
const (
	statusOK          = 0
	statusException   = 1
	statusInterrupted = 2
	statusOOM         = 3
	statusBadInput    = 4
)

// VM is one persistent JS runtime in one wasm instance, owned by exactly one
// JSHook or JSAdmission. Top-level state of the script survives between calls.
// All calls must be serialised by the caller (the registry holds a lock);
// Close must not run during a call.
type VM struct {
	eng    *Engine
	m      api.Module
	limits Limits
	host   map[string]HostFunc
	bound  bool
	// pending is the result of the last host call, until glue.c reads it.
	pending []byte

	alloc, free, outPtr, outLen api.Function
	reseed, stackTop            api.Function
	eval, call, hasExport       api.Function

	broken bool
	closed bool
}

// New starts a fresh VM on the process-wide engine. Zero fields of lim fall
// back to DefaultLimits().
func New(lim Limits) (*VM, error) {
	e, err := defaultEngine()
	if err != nil {
		return nil, err
	}
	return e.NewVM(context.Background(), lim)
}

// instantiate creates an instance of the module. start runs the WASI reactor
// initialisation; a restore from a snapshot skips it, because the snapshot
// already holds its effects.
func (e *Engine) instantiate(ctx context.Context, lim Limits, start bool) (*VM, error) {
	cfg := wazero.NewModuleConfig().
		WithName("").
		WithSysWalltime().
		WithSysNanotime()
	if start {
		cfg = cfg.WithStartFunctions("_initialize")
	} else {
		cfg = cfg.WithStartFunctions()
	}
	m, err := e.rt.InstantiateModule(ctx, e.mod, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate engine.wasm: %w", err)
	}
	f := m.ExportedFunction
	return &VM{
		eng: e, m: m, limits: lim,
		alloc: f("gj_alloc"), free: f("gj_free"),
		outPtr: f("gj_out_ptr"), outLen: f("gj_out_len"),
		eval: f("gj_eval"), call: f("gj_call"), hasExport: f("gj_has_export"),
		reseed: f("gj_reseed"), stackTop: f("gj_update_stack_top"),
	}, nil
}

// NewVM instantiates the module and creates a fresh QuickJS runtime with the
// memory limit of lim.
func (e *Engine) NewVM(ctx context.Context, lim Limits) (*VM, error) {
	lim = lim.WithDefaults()
	vm, err := e.instantiate(ctx, lim, true)
	if err != nil {
		return nil, err
	}
	m := vm.m
	r, err := m.ExportedFunction("gj_init").Call(vm.callCtx(ctx), uint64(lim.MemoryMB)*1024*1024, maxStackBytes)
	if err != nil {
		_ = m.Close(ctx)
		return nil, fmt.Errorf("gj_init: %w", err)
	}
	if r[0] != statusOK {
		_ = m.Close(ctx)
		return nil, fmt.Errorf("gj_init: status %d", r[0])
	}
	if _, err := vm.invoke(ctx, vm.reseed, rand.Uint64()); err != nil {
		_ = m.Close(ctx)
		return nil, fmt.Errorf("gj_reseed: %w", err)
	}
	return vm, nil
}

// Limits returns the limits this VM was started with.
func (vm *VM) Limits() Limits { return vm.limits }

// Usable reports whether the VM can serve another call. A timeout or the
// memory limit leave it usable; a wasm trap (TrapError) does not.
func (vm *VM) Usable() bool { return !vm.broken && !vm.closed }

type vmKey struct{}

// callCtx puts the VM into the context, so the host imports find it. wazero
// hands this context on to them.
func (vm *VM) callCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, vmKey{}, vm)
}

// invoke calls a wasm export. A Go error is a failure of the module: the VM
// is broken afterwards.
func (vm *VM) invoke(ctx context.Context, fn api.Function, args ...uint64) (uint64, error) {
	if vm.closed {
		return 0, ErrClosed
	}
	r, err := fn.Call(vm.callCtx(ctx), args...)
	if err != nil {
		vm.broken = true
		return 0, &TrapError{Err: err}
	}
	if len(r) == 0 {
		return 0, nil
	}
	return r[0], nil
}

// put copies b plus a trailing NUL into wasm memory.
func (vm *VM) put(ctx context.Context, b []byte) (uint32, error) {
	p, err := vm.invoke(ctx, vm.alloc, uint64(len(b)+1))
	if err != nil {
		return 0, err
	}
	if p == 0 {
		return 0, ErrOOM
	}
	mem := vm.m.Memory()
	if !mem.Write(uint32(p), b) || !mem.WriteByte(uint32(p)+uint32(len(b)), 0) {
		vm.broken = true
		return 0, &TrapError{Err: errors.New("write out of range")}
	}
	return uint32(p), nil
}

func (vm *VM) release(ctx context.Context, ptrs ...uint32) {
	for _, p := range ptrs {
		if p != 0 {
			_, _ = vm.invoke(ctx, vm.free, uint64(p))
		}
	}
}

func (vm *VM) out(ctx context.Context) ([]byte, error) {
	p, err := vm.invoke(ctx, vm.outPtr)
	if err != nil {
		return nil, err
	}
	n, err := vm.invoke(ctx, vm.outLen)
	if err != nil || n == 0 {
		return nil, err
	}
	b, ok := vm.m.Memory().Read(uint32(p), uint32(n))
	if !ok {
		vm.broken = true
		return nil, &TrapError{Err: errors.New("output out of range")}
	}
	return append([]byte(nil), b...), nil
}

// result turns the status of a glue.c call into the output or a typed error.
func (vm *VM) result(ctx context.Context, status uint64) ([]byte, error) {
	out, err := vm.out(ctx)
	if err != nil {
		return nil, err
	}
	switch status {
	case statusOK:
		return out, nil
	case statusException, statusBadInput:
		return nil, parseJSError(out)
	case statusInterrupted:
		return nil, cancelled(errors.New(string(out)), ctx.Err())
	case statusOOM:
		return nil, ErrOOM
	}
	vm.broken = true
	return nil, &TrapError{Err: fmt.Errorf("unknown status %d", status)}
}

// Eval runs source as a global script and returns the string form of its last
// expression ("" for undefined). name shows in stack traces. Function
// declarations become globals, so a hook's handle() is callable
// afterwards. A ctx that ends stops the script with ErrCancelled; the VM
// stays usable.
func (vm *VM) Eval(ctx context.Context, name, source string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", cancelled(nil, err)
	}
	src, err := vm.put(ctx, []byte(source))
	if err != nil {
		return "", err
	}
	nm, err := vm.put(ctx, []byte(name))
	if err != nil {
		vm.release(ctx, src)
		return "", err
	}
	defer vm.release(context.WithoutCancel(ctx), src, nm)
	st, err := vm.invoke(ctx, vm.eval, uint64(src), uint64(len(source)), uint64(nm))
	if err != nil {
		return "", err
	}
	out, err := vm.result(ctx, st)
	if err != nil {
		if je, ok := err.(*JSError); ok {
			return "", fmt.Errorf("eval %s: %w", name, je)
		}
		return "", fmt.Errorf("eval %s: %w", name, err)
	}
	return string(out), nil
}

// LoadModule evaluates the user's source. The source is a script, not an ES
// module: a top-level `function handle(ctx) {}` becomes a global. The registry
// snapshots the VM right after, so every call starts from this state
// (js-registry.R5).
func (vm *VM) LoadModule(ctx context.Context, name, source string) error {
	_, err := vm.Eval(ctx, name, source)
	return err
}

// HasExport reports whether the loaded script has a global function called
// name. It runs no user code.
func (vm *VM) HasExport(name string) bool {
	ctx := context.Background()
	p, err := vm.put(ctx, []byte(name))
	if err != nil {
		return false
	}
	defer vm.release(ctx, p)
	r, err := vm.invoke(ctx, vm.hasExport, uint64(p))
	return err == nil && r != 0
}

// CallExport calls the global function name with at most one argument (the
// engine passes one JSON value) and returns the JSON of the result ("" for
// undefined). Errors: ErrCancelled, ErrOOM, *JSError (with message and stack),
// *TrapError. A script error names the export: `calling handle(): ...`.
func (vm *VM) CallExport(ctx context.Context, name string, args ...any) (string, error) {
	var arg []byte
	switch len(args) {
	case 0:
	case 1:
		var err error
		if arg, err = json.Marshal(args[0]); err != nil {
			return "", fmt.Errorf("encode %s() argument: %w", name, err)
		}
	default:
		return "", fmt.Errorf("calling %s(): the engine passes at most one argument, got %d", name, len(args))
	}
	return vm.callJSON(ctx, name, arg)
}

// callJSON is CallExport with an argument that is already JSON (nil for none).
func (vm *VM) callJSON(ctx context.Context, name string, argJSON []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", cancelled(nil, err)
	}
	nm, err := vm.put(ctx, []byte(name))
	if err != nil {
		return "", err
	}
	a, err := vm.put(ctx, argJSON)
	if err != nil {
		vm.release(ctx, nm)
		return "", err
	}
	defer vm.release(context.WithoutCancel(ctx), nm, a)
	st, err := vm.invoke(ctx, vm.call, uint64(nm), uint64(a), uint64(len(argJSON)))
	if err != nil {
		return "", err
	}
	out, err := vm.result(ctx, st)
	if err != nil {
		return "", fmt.Errorf("calling %s(): %w", name, err)
	}
	return string(out), nil
}

// Invoke calls the named export with in as its one argument (none if in is
// nil), marshalled to JSON, and decodes the JSON form of the result into out.
// out stays untouched when the export returned undefined; a nil out discards
// the result. It satisfies jsrun.Script.
func (vm *VM) Invoke(ctx context.Context, export string, in, out any) error {
	var args []any
	if in != nil {
		args = []any{in}
	}
	raw, err := vm.CallExport(ctx, export, args...)
	if err != nil || raw == "" || out == nil {
		return err
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("decode %s() result: %w", export, err)
	}
	return nil
}

// Close releases the instance. It is idempotent. The caller must hold the
// per-VM lock: closing during a call races inside wazero.
func (vm *VM) Close() {
	if vm.closed {
		return
	}
	vm.closed = true
	_ = vm.m.Close(context.Background())
}

// instantiateEnv defines the host module "env" that glue.c imports.
func instantiateEnv(ctx context.Context, rt wazero.Runtime) error {
	_, err := rt.NewHostModuleBuilder("env").
		// interrupt: QuickJS asks every few thousand operations whether the
		// call must stop; the answer is the state of the call's context.
		NewFunctionBuilder().WithFunc(func(ctx context.Context) int32 {
		if ctx.Err() != nil {
			return 1
		}
		return 0
	}).Export("interrupt").
		NewFunctionBuilder().WithFunc(hostCall).Export("host_call").
		NewFunctionBuilder().WithFunc(hostRead).Export("host_read").
		Instantiate(ctx)
	return err
}

// hostCall runs the registered Go function for the script and keeps the JSON
// result for hostRead. It returns the result length, or -(n+1) for an error
// message of n bytes.
func hostCall(ctx context.Context, m api.Module, name, nlen, arg, alen uint32) int32 {
	vm, _ := ctx.Value(vmKey{}).(*VM)
	fail := func(msg string) int32 {
		if vm != nil {
			vm.pending = []byte(msg)
		}
		return -int32(len(msg)) - 1
	}
	if vm == nil {
		return fail("host call outside a VM call")
	}
	mem := m.Memory()
	nb, ok1 := mem.Read(name, nlen)
	ab, ok2 := mem.Read(arg, alen)
	if !ok1 || !ok2 {
		return fail("host call: bad pointer")
	}
	fn := vm.host[string(nb)]
	if fn == nil {
		return fail("unknown host function " + string(nb))
	}
	res, err := fn(ctx, json.RawMessage(append([]byte(nil), ab...)))
	if err != nil {
		return fail(err.Error())
	}
	b, err := json.Marshal(res)
	if err != nil {
		return fail("encode host result: " + err.Error())
	}
	vm.pending = b
	return int32(len(b))
}

// hostRead copies the pending host result or message into the guest.
func hostRead(ctx context.Context, m api.Module, dst uint32) {
	vm, _ := ctx.Value(vmKey{}).(*VM)
	if vm == nil {
		return
	}
	m.Memory().Write(dst, vm.pending)
	vm.pending = nil
}
