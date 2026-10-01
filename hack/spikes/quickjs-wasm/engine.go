// Package quickjswasm is a spike: QuickJS-ng built by us as a WASI reactor,
// run by wazero, with a small JSON ABI (glue/glue.c). It measures whether a
// fresh VM per call ("single shot") is fast enough and which cache makes it
// fast. It is not used by the operator.
package quickjswasm

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed engine.wasm
var engineWasm []byte

// Status codes of glue.c.
const (
	statusOK          = 0
	statusException   = 1
	statusInterrupted = 2
	statusOOM         = 3
	statusBadInput    = 4
)

var (
	ErrInterrupted = errors.New("interrupted")
	ErrOOM         = errors.New("out of memory")
)

// JSError is an exception thrown by the script.
type JSError struct{ Text string }

func (e *JSError) Error() string { return e.Text }

// Engine holds one wazero runtime and the compiled module. Every VM is an
// anonymous instance of that module in this runtime.
type Engine struct {
	rt  wazero.Runtime
	mod wazero.CompiledModule
}

// EngineOptions configure NewEngine.
type EngineOptions struct {
	// Cache shares compiled machine code between runtimes; with a directory
	// it survives a process restart. Nil compiles without cache.
	Cache wazero.CompilationCache
	// MaxMemoryPages caps the linear memory of each VM (64 KiB pages). It is
	// a hard cap below the QuickJS memory limit.
	MaxMemoryPages uint32
	// Interpreter uses the wazero interpreter instead of the compiler.
	Interpreter bool
	// CloseOnContextDone makes wazero check the context in compiled code and
	// close the module when it ends (today's operator setting).
	CloseOnContextDone bool
}

// NewEngine compiles engine.wasm.
func NewEngine(ctx context.Context, o EngineOptions) (*Engine, error) {
	cfg := wazero.NewRuntimeConfigCompiler()
	if o.Interpreter {
		cfg = wazero.NewRuntimeConfigInterpreter()
	}
	if o.Cache != nil {
		cfg = cfg.WithCompilationCache(o.Cache)
	}
	cfg = cfg.WithCloseOnContextDone(o.CloseOnContextDone)
	if o.MaxMemoryPages > 0 {
		cfg = cfg.WithMemoryLimitPages(o.MaxMemoryPages)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return nil, err
	}
	// QuickJS polls this; wazero hands the host function the context of
	// the running call, so a passed deadline stops the script without
	// closing the module.
	_, err := rt.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context) int32 {
			if ctx.Err() != nil {
				return 1
			}
			return 0
		}).
		Export("interrupt").
		Instantiate(ctx)
	if err != nil {
		return nil, err
	}
	mod, err := rt.CompileModule(ctx, engineWasm)
	if err != nil {
		return nil, err
	}
	return &Engine{rt: rt, mod: mod}, nil
}

// Close frees the runtime and every VM in it.
func (e *Engine) Close(ctx context.Context) error { return e.rt.Close(ctx) }

// Limits of one VM.
type Limits struct {
	MemoryBytes uint32
	StackBytes  uint32
}

// VM is one QuickJS runtime in one wasm instance. Not goroutine-safe.
type VM struct {
	m                                         api.Module
	alloc, free, outPtr, outLen               api.Function
	eval, compile, evalBC, call, memUsed, ust api.Function
}

func (e *Engine) instantiate(ctx context.Context, start bool) (*VM, error) {
	cfg := wazero.NewModuleConfig().WithName("")
	if start {
		cfg = cfg.WithStartFunctions("_initialize")
	} else {
		cfg = cfg.WithStartFunctions()
	}
	m, err := e.rt.InstantiateModule(ctx, e.mod, cfg)
	if err != nil {
		return nil, err
	}
	f := m.ExportedFunction
	return &VM{
		m: m, alloc: f("gj_alloc"), free: f("gj_free"),
		outPtr: f("gj_out_ptr"), outLen: f("gj_out_len"),
		eval: f("gj_eval"), compile: f("gj_compile"), evalBC: f("gj_eval_bytecode"),
		call: f("gj_call"), memUsed: f("gj_mem_used"), ust: f("gj_update_stack_top"),
	}, nil
}

// NewVM instantiates the module and creates a fresh QuickJS runtime.
func (e *Engine) NewVM(ctx context.Context, l Limits) (*VM, error) {
	vm, err := e.instantiate(ctx, true)
	if err != nil {
		return nil, err
	}
	res, err := vm.init(ctx, l)
	if err != nil {
		_ = vm.Close(ctx)
		return nil, err
	}
	if res != statusOK {
		_ = vm.Close(ctx)
		return nil, fmt.Errorf("gj_init: status %d", res)
	}
	return vm, nil
}

func (vm *VM) init(ctx context.Context, l Limits) (uint64, error) {
	r, err := vm.m.ExportedFunction("gj_init").Call(ctx, uint64(l.MemoryBytes), uint64(l.StackBytes))
	if err != nil {
		return 0, err
	}
	return r[0], nil
}

// Snapshot is the linear memory of a VM between calls. Data has its trailing
// zero bytes cut off; Pages is the full size, which a restore must grow back
// to, because the allocator reads the heap end from the memory size.
type Snapshot struct {
	Pages uint32
	Data  []byte
}

// NewVMFromSnapshot instantiates the module without running its start
// function and overwrites its linear memory with snap. glue.c keeps all state
// in linear memory, and the only mutable wasm global (the stack pointer) is
// at its base between calls, so the memory is the whole VM state.
func (e *Engine) NewVMFromSnapshot(ctx context.Context, snap *Snapshot) (*VM, error) {
	vm, err := e.instantiate(ctx, false)
	if err != nil {
		return nil, err
	}
	mem := vm.m.Memory()
	if have := mem.Size() / 65536; have < snap.Pages {
		if _, ok := mem.Grow(snap.Pages - have); !ok {
			_ = vm.Close(ctx)
			return nil, ErrOOM
		}
	}
	if !mem.Write(0, snap.Data) {
		_ = vm.Close(ctx)
		return nil, errors.New("snapshot does not fit")
	}
	if _, err := vm.ust.Call(ctx); err != nil {
		_ = vm.Close(ctx)
		return nil, err
	}
	return vm, nil
}

// Snapshot copies the linear memory. Take it only between calls.
func (vm *VM) Snapshot() *Snapshot {
	mem := vm.m.Memory()
	b, _ := mem.Read(0, mem.Size())
	n := len(b)
	for n > 0 && b[n-1] == 0 {
		n--
	}
	return &Snapshot{Pages: mem.Size() / 65536, Data: append([]byte(nil), b[:n]...)}
}

// MemoryBytes is the size of the linear memory.
func (vm *VM) MemoryBytes() uint32 { return vm.m.Memory().Size() }

// Close frees the instance.
func (vm *VM) Close(ctx context.Context) error { return vm.m.Close(ctx) }

// put copies b plus a trailing NUL into wasm memory.
func (vm *VM) put(ctx context.Context, b []byte) (uint32, error) {
	r, err := vm.alloc.Call(ctx, uint64(len(b)+1))
	if err != nil {
		return 0, err
	}
	p := uint32(r[0])
	if p == 0 {
		return 0, ErrOOM
	}
	mem := vm.m.Memory()
	if !mem.Write(p, b) || !mem.WriteByte(p+uint32(len(b)), 0) {
		return 0, errors.New("write out of range")
	}
	return p, nil
}

func (vm *VM) release(ctx context.Context, ptrs ...uint32) {
	for _, p := range ptrs {
		_, _ = vm.free.Call(ctx, uint64(p))
	}
}

func (vm *VM) out(ctx context.Context) ([]byte, error) {
	p, err := vm.outPtr.Call(ctx)
	if err != nil {
		return nil, err
	}
	n, err := vm.outLen.Call(ctx)
	if err != nil {
		return nil, err
	}
	if n[0] == 0 {
		return nil, nil
	}
	b, ok := vm.m.Memory().Read(uint32(p[0]), uint32(n[0]))
	if !ok {
		return nil, errors.New("output out of range")
	}
	return append([]byte(nil), b...), nil
}

func (vm *VM) result(ctx context.Context, status uint64) ([]byte, error) {
	out, err := vm.out(ctx)
	if err != nil {
		return nil, err
	}
	switch status {
	case statusOK:
		return out, nil
	case statusException, statusBadInput:
		return nil, &JSError{Text: string(out)}
	case statusInterrupted:
		return nil, ErrInterrupted
	case statusOOM:
		return nil, ErrOOM
	}
	return nil, fmt.Errorf("unknown status %d", status)
}

// Eval runs source as a global script.
func (vm *VM) Eval(ctx context.Context, name, source string) error {
	src, err := vm.put(ctx, []byte(source))
	if err != nil {
		return err
	}
	nm, err := vm.put(ctx, []byte(name))
	if err != nil {
		return err
	}
	defer vm.release(ctx, src, nm)
	r, err := vm.eval.Call(ctx, uint64(src), uint64(len(source)), uint64(nm))
	if err != nil {
		return err
	}
	_, err = vm.result(ctx, r[0])
	return err
}

// Compile returns the bytecode of source without running it. stripSource
// drops the source text from the bytecode; line numbers stay.
func (vm *VM) Compile(ctx context.Context, name, source string, stripSource bool) ([]byte, error) {
	src, err := vm.put(ctx, []byte(source))
	if err != nil {
		return nil, err
	}
	nm, err := vm.put(ctx, []byte(name))
	if err != nil {
		return nil, err
	}
	defer vm.release(ctx, src, nm)
	strip := uint64(0)
	if stripSource {
		strip = 1
	}
	r, err := vm.compile.Call(ctx, uint64(src), uint64(len(source)), uint64(nm), strip)
	if err != nil {
		return nil, err
	}
	return vm.result(ctx, r[0])
}

// EvalBytecode runs bytecode from Compile.
func (vm *VM) EvalBytecode(ctx context.Context, bc []byte) error {
	p, err := vm.put(ctx, bc)
	if err != nil {
		return err
	}
	defer vm.release(ctx, p)
	r, err := vm.evalBC.Call(ctx, uint64(p), uint64(len(bc)))
	if err != nil {
		return err
	}
	_, err = vm.result(ctx, r[0])
	return err
}

// Call calls the global function fn with one JSON argument and returns the
// JSON of its result.
func (vm *VM) Call(ctx context.Context, fn string, argJSON []byte) ([]byte, error) {
	nm, err := vm.put(ctx, []byte(fn))
	if err != nil {
		return nil, err
	}
	a, err := vm.put(ctx, argJSON)
	if err != nil {
		return nil, err
	}
	defer vm.release(ctx, nm, a)
	r, err := vm.call.Call(ctx, uint64(nm), uint64(a), uint64(len(argJSON)))
	if err != nil {
		return nil, err
	}
	return vm.result(ctx, r[0])
}

// MemUsed is the QuickJS heap in bytes.
func (vm *VM) MemUsed(ctx context.Context) (uint32, error) {
	r, err := vm.memUsed.Call(ctx)
	if err != nil {
		return 0, err
	}
	return uint32(r[0]), nil
}
