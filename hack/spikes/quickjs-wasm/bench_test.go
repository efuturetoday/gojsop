package quickjswasm

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/fastschema/qjs"
	"github.com/tetratelabs/wazero"
)

// A single shot builds a VM, loads the policy, calls validate once and
// closes the VM. The compiled wasm module is cached in the process in every
// variant; BenchmarkStartup covers the first compile.

type policy struct {
	name, src string
}

func policies(b *testing.B) []policy {
	return []policy{{"small", smallPolicy}, {"large", largePolicy(b)}}
}

// qjsSingleShot is today's path: jsengine.New plus LoadModule plus the
// admission Invoke, with the request as a Go value.
func qjsSingleShot(b *testing.B, src string, req map[string]any) {
	ctx := context.Background()
	rt, err := qjs.New(qjs.Option{
		MemoryLimit:        32 << 20,
		MaxStackSize:       512 << 10,
		Context:            ctx,
		CloseOnContextDone: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer rt.Close()
	v, err := rt.Context().Eval("policy.js", qjs.Code(src))
	if err != nil {
		b.Fatal(err)
	}
	v.Free()
	out, err := rt.Context().Global().Invoke("validate", req)
	if err != nil {
		b.Fatal(err)
	}
	s, err := out.JSONStringify()
	out.Free()
	if err != nil || s != `{"allowed":true}` {
		b.Fatalf("%s %v", s, err)
	}
}

func BenchmarkSingleShot(b *testing.B) {
	ctx := context.Background()
	reqJSON := admissionRequest(b)
	var reqMap map[string]any
	if err := json.Unmarshal(reqJSON, &reqMap); err != nil {
		b.Fatal(err)
	}
	e := newTestEngine(b)
	call := func(b *testing.B, vm *VM) {
		out, err := vm.Call(ctx, "validate", reqJSON)
		if err != nil || string(out) != `{"allowed":true}` {
			b.Fatalf("%s %v", out, err)
		}
		_ = vm.Close(ctx)
	}

	for _, p := range policies(b) {
		b.Run("qjs-fastschema/"+p.name, func(b *testing.B) {
			for b.Loop() {
				qjsSingleShot(b, p.src, reqMap)
			}
		})
		b.Run("own-source/"+p.name, func(b *testing.B) {
			for b.Loop() {
				vm, err := e.NewVM(ctx, testLimits)
				if err != nil {
					b.Fatal(err)
				}
				if err := vm.Eval(ctx, "policy.js", p.src); err != nil {
					b.Fatal(err)
				}
				call(b, vm)
			}
		})
		for _, strip := range []bool{false, true} {
			bc, err := newTestVM(b, e).Compile(ctx, "policy.js", p.src, strip)
			if err != nil {
				b.Fatal(err)
			}
			variant := "own-bytecode/"
			if strip {
				variant = "own-bytecode-stripped/"
			}
			b.Run(variant+p.name, func(b *testing.B) {
				b.ReportMetric(float64(len(bc)), "cache-B")
				for b.Loop() {
					vm, err := e.NewVM(ctx, testLimits)
					if err != nil {
						b.Fatal(err)
					}
					if err := vm.EvalBytecode(ctx, bc); err != nil {
						b.Fatal(err)
					}
					call(b, vm)
				}
			})
		}
		base := newTestVM(b, e)
		if err := base.Eval(ctx, "policy.js", p.src); err != nil {
			b.Fatal(err)
		}
		snap := base.Snapshot()
		b.Run("own-snapshot/"+p.name, func(b *testing.B) {
			b.ReportMetric(float64(len(snap.Data)), "cache-B")
			for b.Loop() {
				vm, err := e.NewVMFromSnapshot(ctx, snap)
				if err != nil {
					b.Fatal(err)
				}
				call(b, vm)
			}
		})
		b.Run("own-snapshot-parallel/"+p.name, func(b *testing.B) {
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					vm, err := e.NewVMFromSnapshot(ctx, snap)
					if err != nil {
						b.Error(err)
						return
					}
					out, err := vm.Call(ctx, "validate", reqJSON)
					if err != nil || string(out) != `{"allowed":true}` {
						b.Errorf("%s %v", out, err)
					}
					_ = vm.Close(ctx)
				}
			})
		})
	}
}

// BenchmarkWarmCall is today's long-lived VM: one call on a loaded VM.
func BenchmarkWarmCall(b *testing.B) {
	ctx := context.Background()
	reqJSON := admissionRequest(b)
	var reqMap map[string]any
	if err := json.Unmarshal(reqJSON, &reqMap); err != nil {
		b.Fatal(err)
	}
	e := newTestEngine(b)
	for _, p := range policies(b) {
		b.Run("qjs-fastschema/"+p.name, func(b *testing.B) {
			rt, err := qjs.New(qjs.Option{MemoryLimit: 32 << 20, MaxStackSize: 512 << 10, Context: ctx, CloseOnContextDone: true})
			if err != nil {
				b.Fatal(err)
			}
			defer rt.Close()
			v, err := rt.Context().Eval("policy.js", qjs.Code(p.src))
			if err != nil {
				b.Fatal(err)
			}
			v.Free()
			for b.Loop() {
				out, err := rt.Context().Global().Invoke("validate", reqMap)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := out.JSONStringify(); err != nil {
					b.Fatal(err)
				}
				out.Free()
			}
		})
		b.Run("own/"+p.name, func(b *testing.B) {
			vm := newTestVM(b, e)
			if err := vm.Eval(ctx, "policy.js", p.src); err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				if _, err := vm.Call(ctx, "validate", reqJSON); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(vm.MemoryBytes()), "linmem-B")
		})
	}
}

// BenchmarkStartup is the first VM of a process: compile engine.wasm to
// machine code, with no cache and with a warm cache directory (a restarted
// pod with the cache on disk).
func BenchmarkStartup(b *testing.B) {
	ctx := context.Background()
	b.Run("compile-nocache", func(b *testing.B) {
		for b.Loop() {
			e, err := NewEngine(ctx, EngineOptions{})
			if err != nil {
				b.Fatal(err)
			}
			_ = e.Close(ctx)
		}
	})
	dir, err := os.MkdirTemp(b.TempDir(), "wazero")
	if err != nil {
		b.Fatal(err)
	}
	warm, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		b.Fatal(err)
	}
	e, err := NewEngine(ctx, EngineOptions{Cache: warm})
	if err != nil {
		b.Fatal(err)
	}
	_ = e.Close(ctx)
	b.Run("compile-dircache", func(b *testing.B) {
		for b.Loop() {
			// A new cache object on the same directory is a new process.
			c, err := wazero.NewCompilationCacheWithDir(dir)
			if err != nil {
				b.Fatal(err)
			}
			e, err := NewEngine(ctx, EngineOptions{Cache: c})
			if err != nil {
				b.Fatal(err)
			}
			_ = e.Close(ctx)
			_ = c.Close(ctx)
		}
	})
	b.Run("interpreter-nocompile", func(b *testing.B) {
		for b.Loop() {
			e, err := NewEngine(ctx, EngineOptions{Interpreter: true})
			if err != nil {
				b.Fatal(err)
			}
			_ = e.Close(ctx)
		}
	})
}

// BenchmarkCloseOnContextDone measures what the wazero context check costs
// in compiled code (EXEC-3): the same work with the option off and on.
func BenchmarkCloseOnContextDone(b *testing.B) {
	ctx := context.Background()
	reqJSON := admissionRequest(b)
	src := largePolicy(b)
	loop := `function busy(n) { let s = 0; for (let i = 0; i < 2000000; i++) s += i % 7; return s; }`
	for _, on := range []bool{false, true} {
		name := "off"
		if on {
			name = "on"
		}
		e, err := NewEngine(ctx, EngineOptions{CloseOnContextDone: on})
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = e.Close(ctx) }()
		vm, err := e.NewVM(ctx, testLimits)
		if err != nil {
			b.Fatal(err)
		}
		if err := vm.Eval(ctx, "p", src+loop); err != nil {
			b.Fatal(err)
		}
		b.Run(name+"/validate-large", func(b *testing.B) {
			for b.Loop() {
				if _, err := vm.Call(ctx, "validate", reqJSON); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/busy-loop-2M", func(b *testing.B) {
			for b.Loop() {
				if _, err := vm.Call(ctx, "busy", []byte(`0`)); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/eval-lodash", func(b *testing.B) {
			for b.Loop() {
				v, err := e.NewVM(ctx, testLimits)
				if err != nil {
					b.Fatal(err)
				}
				if err := v.Eval(ctx, "p", src); err != nil {
					b.Fatal(err)
				}
				_ = v.Close(ctx)
			}
		})
	}
}
