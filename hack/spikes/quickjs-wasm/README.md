# Spike: own QuickJS-ng wasm build

Question: can we replace `fastschema/qjs` with our own QuickJS build, so that
a fresh VM per call ("single shot") is fast enough, and which cache makes it
fast? Backlog: EXEC-8 (done), EXEC-9 (single shot, open).

**Status after EXEC-8:** the engine, the glue, the interrupt handler, the memory
limit and the compilation cache moved to `internal/jsengine` and became rules
in `.agents/aspects/js-execution.md`. This module stays as evidence and as the
only home of what the operator does not use: bytecode (`gj_compile`,
`gj_eval_bytecode`), memory snapshots and the comparison benchmarks against
`fastschema/qjs` (its own `go.mod` keeps that old dependency). EXEC-9 reads it.
Its `glue/glue.c` is the spike's, not the operator's.

This directory is a separate Go module. The operator does not use it, and
`make test lint` of the root module does not see it.

## What is here

- `glue/glue.c`: the whole ABI, about 200 lines. One wasm instance holds one
  QuickJS runtime and context. Values cross as JSON; Go never holds a
  `JSValue`. The script is a global script, like `jsengine.LoadModule`.
- `glue/build.sh`: builds `engine.wasm` (QuickJS-ng v0.17.0, wasi-sdk 34,
  `-O2 -flto`, `wasm-opt -O3`, 961 KB) as a WASI reactor.
- `engine.go`: the Go side on wazero v1.9.0 (the operator's version). One
  `wazero.Runtime` per process, every VM is an anonymous module instance.
- `engine_test.go`: behaviour; `bench_test.go`: the numbers below.

Build and run:

```bash
WASI_SDK=/path/to/wasi-sdk-34 QUICKJS=/path/to/quickjs-ng ./glue/build.sh
go test ./... && go test -run '^$' -bench . -benchmem
```

## Results

Apple M1 Pro, Go 1.26.1, wazero v1.9.0 (compiler). Request: a pod CREATE
admission request of about 4 KB. Policies: "small" is the sample
`prevent-latest-tags` policy; "large" is the same check on top of lodash
(544 KB unminified), the size of a policy bundled with a library.

### Single shot: build VM, load policy, call `validate`, close

| Variant | small | large | cache per policy |
|---|---|---|---|
| `fastschema/qjs` (today's path) | 5.41 ms | 107.2 ms | none |
| own, from source | 0.47 ms | 21.7 ms | none |
| own, from bytecode | 0.44 ms | 4.0 ms | large 1.37 MB |
| own, from bytecode without source text | 0.40 ms | 3.3 ms | large 124 KB |
| own, from memory snapshot | **0.28 ms** | **0.44 ms** | small 1.3 MB, large 4.0 MB |
| own, from snapshot, 10 goroutines (time per shot) | 0.14 ms | 0.17 ms | |

### Warm call on a long-lived VM (today's model)

| | small | large |
|---|---|---|
| `fastschema/qjs` | 1.04 ms, 5727 allocs | 1.10 ms, 5729 allocs |
| own (JSON ABI) | 65 µs, 14 allocs | 83 µs, 14 allocs |

`qjs` converts the request field by field through many wasm calls
(`Invoke` with a Go value). One `JSON.parse` inside wasm is about 16 times
faster.

### Cost of `CloseOnContextDone` (EXEC-3)

Same own engine, option off and on:

| | off | on | factor |
|---|---|---|---|
| warm `validate`, large | 81 µs | 304 µs | 3.8× |
| busy loop, 2M iterations | 140 ms | 996 ms | 7.1× |
| eval lodash | 22 ms | 114 ms | 5.1× |

This explains most of the gap to `qjs`. With a deadline context and the
interrupt handler, the busy loop stays at 136 ms: polling `ctx.Err()` every
10000 ops costs nothing measurable.

### First VM of a process

| | time |
|---|---|
| compile `engine.wasm`, no cache | 318 ms |
| compile with a warm `wazero.NewCompilationCacheWithDir` | 13 ms |
| wazero interpreter, no compile | 31 ms (but every call much slower) |

## Findings

1. **Single shot is fast with the own build.** Even from source, the small
   policy takes 0.47 ms. A snapshot makes the size of the policy almost
   irrelevant: 0.28 ms small, 0.44 ms large.
2. **Cancellation without killing the VM works.** The QuickJS interrupt
   handler asks the host (`env.interrupt`), the host answers from the call's
   `ctx.Err()`. An endless loop stops at the deadline with `ErrInterrupted`;
   `try/catch` cannot catch it; the VM keeps its state and serves the next
   call. `CloseOnContextDone` is no longer needed, and a timeout needs no
   rebuild (today it always does, js-registry REG-4).
3. **The memory limit leaves the VM usable.** `JS_SetMemoryLimit` ends the
   call with `ErrOOM`, and the next call succeeds. wazero's
   `WithMemoryLimitPages` stays as a hard cap below it.
4. **Exceptions carry message and stack with file and line** (EXEC-4).
5. **Caching, three layers:**
   - Machine code: `wazero.NewCompilationCacheWithDir` on an `emptyDir`
     cuts the first VM after a restart from 318 ms to 13 ms. Key: wazero
     does it by module hash.
   - Per policy, portable: bytecode (`JS_WriteObject`). 5–6 times faster
     than source for a large policy. Without the source text
     (`JS_WRITE_OBJ_STRIP_SOURCE`, line numbers stay) it shrinks from
     1.37 MB to 124 KB and loads faster; only `Function.prototype.toString`
     loses the text. It is bound to the exact
     `engine.wasm` build, and `JS_ReadObject` does not validate its input,
     so the operator must only read bytecode it wrote itself. Never take it
     from a CR or ConfigMap.
   - Per policy, in process: a snapshot of the linear memory after the
     top-level eval. Fastest. Restoring works because `glue.c` keeps all
     state in linear memory and the only mutable wasm global (the stack
     pointer) is at its base between calls; the restore must grow the memory
     back to the snapshot's page count, because the allocator reads the heap
     end from the memory size. Key: engine build hash, source hash, limits.
     Cost: 1.3–4 MB of Go heap per policy; most of the small one is the
     zeroed 1 MB stack, so a sparse snapshot (skip zero pages) would shrink
     it.
6. **Snapshot gap: all restored VMs share one random state.** Two VMs from
   one snapshot return the same `Math.random()`, and they share
   `hash_seed`. QuickJS-ng has no API to reseed; the own build needs a small
   patch (`JS_SetRandomSeed`) and a reseed after every restore. Not done in
   the spike.
7. **What breaks in single shot:** top-level state no longer survives
   between calls (js-registry.R5, js-execution "Runtimes are long-lived").
   With a snapshot, every call starts from the state after the top-level
   eval. A hook that counts or caches across calls behaves differently.
   This is a product decision, not a technical one.

## Not covered

- Host functions (`kube.get` and friends): only `env.interrupt` exists.
  The JSON ABI fits them (one import `env.host_call(name, json) -> json`).
- ES modules (`import`/`export`) and promises/jobs (`JS_ExecutePendingJob`).
- Linux/amd64 numbers; CI runs there.
- Memory per idle VM in the long-lived model: small 1.3 MB, large 4.0 MB of
  linear memory.
