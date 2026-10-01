// Package jsengine runs the JavaScript of each JSHook and JSAdmission in
// QuickJS-ng, compiled by us to WebAssembly (glue/glue.c, engine.wasm) and run
// by wazero inside the operator process. It is the only package that imports
// wazero (js-execution.R1).
//
// The engine is feature-agnostic: it knows nothing about JSHook bindings or
// JSAdmission requests. Feature packages build typed wrappers over
// VM.Invoke. Values cross as JSON; host functions (kube.*) cross through one
// import, env.host_call.
package jsengine

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/o-haase/gojsop/internal/jsrun"
)

// engineWasm is built by `make engine-wasm` from glue/glue.c and committed
// (toolchain versions: glue/versions.env).
//
//go:embed engine.wasm
var engineWasm []byte

// Limits caps what a single VM may consume; see jsrun.Limits. The heap limit
// is JS_SetMemoryLimit inside the VM; the deadline is the context of each call,
// answered by the QuickJS interrupt handler.
type Limits = jsrun.Limits

// DefaultLimits matches the CRD doc defaults (memoryMB:32, timeoutSeconds:30).
func DefaultLimits() Limits { return jsrun.DefaultLimits() }

const (
	// maxStackBytes is the QuickJS stack cap. The wasm stack is 1 MiB
	// (glue/build.sh); half of it is plenty for hook code and leaves room for
	// the C code below the script.
	maxStackBytes = 512 * 1024

	// MaxMemoryMB is the largest heap limit the CRD allows
	// (api/v1alpha1.JSLimits.MemoryMB). hardCapMB adds room for what lives in
	// the instance next to the QuickJS heap (stack, buffers, allocator slack).
	// wazero fixes the memory cap per runtime, not per instance, so this is
	// one process-wide ceiling below which JS_SetMemoryLimit does the per-VM work.
	MaxMemoryMB = 512
	hardCapMB   = MaxMemoryMB + 64
)

// Options configure an Engine.
type Options struct {
	// CacheDir keeps the machine code that wazero compiles from engine.wasm,
	// so the first VM after a restart skips the compilation (about 320 ms
	// become about 15 ms). Empty keeps the cache in memory only. The directory
	// must be private to the operator: wazero runs what it finds there.
	CacheDir string
}

// Engine is one wazero runtime with the compiled module. Every VM is an
// anonymous instance of that module. One Engine exists per process.
type Engine struct {
	rt    wazero.Runtime
	mod   wazero.CompiledModule
	cache wazero.CompilationCache
}

// NewEngine compiles engine.wasm (or loads the machine code from the cache)
// and registers the host imports.
func NewEngine(ctx context.Context, o Options) (*Engine, error) {
	var cache wazero.CompilationCache
	if o.CacheDir != "" {
		c, err := wazero.NewCompilationCacheWithDir(o.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("compilation cache %q: %w", o.CacheDir, err)
		}
		cache = c
	} else {
		cache = wazero.NewCompilationCache()
	}
	// No CloseOnContextDone: a call ends through the interrupt handler, and the
	// module stays usable (js-execution.R3).
	cfg := wazero.NewRuntimeConfigCompiler().
		WithCompilationCache(cache).
		WithMemoryLimitPages(hardCapMB * 16)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	closeAll := func() { _ = rt.Close(ctx); _ = cache.Close(ctx) }
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		closeAll()
		return nil, err
	}
	if err := instantiateEnv(ctx, rt); err != nil {
		closeAll()
		return nil, err
	}
	mod, err := rt.CompileModule(ctx, engineWasm)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("compile engine.wasm: %w", err)
	}
	return &Engine{rt: rt, mod: mod, cache: cache}, nil
}

// Close frees the runtime and every VM in it.
func (e *Engine) Close(ctx context.Context) error {
	err := e.rt.Close(ctx)
	return errors.Join(err, e.cache.Close(ctx))
}

var (
	defaultMu  sync.Mutex
	defaultEng *Engine
)

// Configure creates the process-wide engine that New uses. Call it once at
// start-up, before the first VM, so the compilation happens (or is loaded from
// the cache) there and not in the first call. Without it New creates the
// engine on first use with an in-memory cache.
func Configure(o Options) error {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultEng != nil {
		return errors.New("jsengine: already configured")
	}
	e, err := NewEngine(context.Background(), o)
	if err != nil {
		return err
	}
	defaultEng = e
	return nil
}

func defaultEngine() (*Engine, error) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultEng == nil {
		e, err := NewEngine(context.Background(), Options{})
		if err != nil {
			return nil, err
		}
		defaultEng = e
	}
	return defaultEng, nil
}
