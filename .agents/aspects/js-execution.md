---
id: js-execution
status: accepted
entrypoints:
  - jsengine.New
  - jsengine.NewEngine
  - jsengine.Configure
  - jsengine.VM.CallExport
  - jsengine.VM.Invoke
  - jsengine.VM.BindHost
  - jsengine.VM.Snapshot
  - jsengine.Snapshot.NewVM
  - jsrun.Runner.Invoke
  - jsrun.Scripts.Ensure
  - jsregistry.Registry.Call
---

# JS Execution

This aspect describes how gojsop executes user JavaScript inside the cluster.

Every JSHook and every JSAdmission gets its own JavaScript runtime. The runtime
is QuickJS-ng, compiled by us to WebAssembly (`internal/jsengine/glue/glue.c`,
embedded as `internal/jsengine/engine.wasm`) and run by wazero inside the
operator process. One `wazero.Runtime` exists per process (`jsengine.Engine`);
each VM is an anonymous instance of the compiled module, with one QuickJS
runtime inside. Four properties follow from this choice:

- **No cgo.** The operator builds with `CGO_ENABLED=0`. wazero runs
  WebAssembly in pure Go, so the engine needs no C toolchain at build time of
  the operator and no native library in the image. `engine.wasm` is committed;
  `make engine-wasm` rebuilds it with a pinned toolchain (R14).
- **Isolation.** Each runtime is a separate WebAssembly instance with its own
  heap. One script cannot read or damage the memory of another, and the memory
  limit applies to each script on its own.
- **Cancellation without killing the VM.** QuickJS asks the host every few
  thousand operations whether the call must stop (`env.interrupt`); the host
  answers from the context of the call. A script in an endless loop ends at the
  deadline with `jsengine.ErrCancelled`, `try/catch` cannot swallow it, and the
  VM keeps its state and serves the next call (R13).
- **JSON at the border.** Values cross as JSON, with one `JSON.parse` and one
  `JSON.stringify` inside wasm; Go never holds a JavaScript value. Host
  functions cross through one import, `env.host_call(name, json) -> json`
  (R11).

Every call runs single shot. When a script is prepared, its module is loaded
into a fresh VM and the top-level code runs once; then the registry takes a
sparse memory snapshot of that VM (`jsengine.VM.Snapshot`: only the pages that
differ from a fresh instance). Every call restores its own VM from the
snapshot (`jsengine.Snapshot.NewVM`, about 0.24 ms), runs one export and
closes it. Top-level state does not survive between calls; a hook that needs
state across calls keeps it in the cluster. `Math.random` is reseeded on every
restore (R16). The [js-registry](js-registry.md) aspect covers how calls get
their VM.

A call ends when its context ends. The execution deadline comes from
`spec.limits.timeoutSeconds`, the heap limit from `spec.limits.memoryMB`. The
defaults are 30 s and 32 MB (`jsengine.DefaultLimits`). A timeout, the memory
limit or a wasm trap (`jsengine.TrapError`: a trap, or a panic in a host
function) end the call; its VM is thrown away in any case, and the next call
starts from the snapshot.

Scripts reach the outside world only through the global object `kube`. Hooks
get `apply`, `get`, `list` and `delete`. Admission policies get read-only
`get` and `list`. Nothing else is bound: no network, no file system, no
timers. The [kube-access](kube-access.md) aspect covers the details.

The ABI of `engine.wasm` is small. Exports: `gj_init(mem_limit, stack_size)`,
`gj_eval`, `gj_call(name, json)`, `gj_has_export`, the output buffer
(`gj_out_ptr`, `gj_out_len`), `gj_alloc`/`gj_free`, and for restores from a
snapshot `gj_reseed(seed)` and `gj_update_stack_top()`. Every call returns a
status: 0 ok, 1 script exception (output: `message\nstack`), 2 interrupted,
3 out of memory, 4 bad input. Imports (module `env`): `interrupt`, `host_call`,
`host_read`. `glue.c` documents them.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once? | `jsengine.Engine` (one wazero runtime, compiled `engine.wasm`, compilation cache), `jsengine.New` and `jsengine.VM` (the only wrapper of wazero and the ABI), `jsengine.Host` for host functions (`Func` for a one-argument host call, `Shim` for JS that wraps it, as `jslog` does for the variadic `console`); run through the port `jsrun.Runner.Invoke` (data path) and prepared through `jsrun.Scripts.Ensure` (lifecycle path), implemented by `jsregistry.Registry.Call`. No second way: `jsadmission` decodes the result from JSON (`jsengine.VM.Invoke`). |
| example | Which real use should others copy? | `jshook.Handle` and `jsadmission.Handle`, typed wrappers over `jsrun.Runner.Invoke`. |
| test helper | How does a test use the aspect without effort? | `jsengine.New` with `jsengine.Limits{}` builds a real VM in a test (the process-wide engine starts on first use); `kubehost` tests use the helper `newKubeHost`. No shared harness package (searched `_test.go` for helpers and fakes). |
| sides | Which sides does it touch? | The operator process (Go) and the C glue compiled into `engine.wasm`, which the operator embeds. |
| tie | How do the sides stay in step? | The CRD defaults for `spec.limits` and `jsengine.DefaultLimits` are tied by `TestDefaultLimits_MatchKubebuilderTags`. The committed `engine.wasm` and `glue.c` are tied by the pinned build `make engine-wasm` (R14); a CI check that a rebuild matches is missing → GATE-27. |

## How to use it

Add a new kind of resource that runs JavaScript:

1. Build its VM only through `jsrun.Scripts.Ensure` with a `jsrun.Spec` that has explicit `jsrun.Limits`, and never wait for the build (js-registry.R15).
2. Run its script only through `jsrun.Runner.Invoke`; the adapter applies the deadline from `spec.limits.timeoutSeconds` (R2, R3). Import `jsrun`, not `jsengine` or `jsregistry` (R10).
3. Classify errors with `errors.Is` against `jsengine.ErrCancelled` and `jsengine.ErrOOM` (R4).
4. Choose the `kube` surface from `kubehost.Factory`: read-only for decisions (R6).
5. Gate: `TestMemoryLimit_Honoured` and `TestSharedFactory_ForAdmission_ReadOnlySurface`.

Change the engine itself: edit `glue/glue.c`, run `make engine-wasm`, commit
the new `engine.wasm` with it, run `make test` and
`go test -run '^$' -bench . ./internal/jsengine/` (EXEC-3 guard: a warm call
stays at tens of microseconds, a timeout costs its deadline).

## Rules

- **R1** Only package `jsengine` imports wazero, and nobody imports `fastschema/qjs`.
  Why: the engine stays replaceable, and error handling stays in one place.
  Gate: `TestImportBoundary_EngineStaysBehindRegistry`.
- **R2** Run user JavaScript only through `jsrun.Runner.Invoke`, which the registry runs inside `Registry.Call`. The only
  exception is building a VM inside the registry (module load and `config()`).
  Why: one place for the call semaphore, panic recovery and result classification.
  Gate: `TestImportBoundary_CallersUseOnlyRunnerPort`, `TestRegistry_Invoke_ClassifiesOutcomes`.
- **R3** Give every call, and every build (module load and `config()`), a context with a deadline from
  `spec.limits.timeoutSeconds`; the adapter applies it inside `Runner.Invoke`, so the dispatcher and the admission server (which add a tighter deadline of their own) need not know the limit.
  Why: without a deadline an endless loop holds the VM forever. A build that runs into its deadline leaves the hook `Ready=False` with reason `BuildFailed` (status-conditions.R7).
  Gate: `TestDispatcher_Timeout_CancelsWarnsAndKeepsVM`, `TestRegistry_Invoke_EveryOutcomeGetsAFreshInstance`. The build deadline: `TestRegistry_Ensure_HangingBuildDoesNotBlockOtherKeys`.
- **R4** Classify engine errors with `errors.Is` against `jsengine.ErrCancelled` and `jsengine.ErrOOM`, and with `errors.As` against `*jsengine.JSError` (a script exception) and `*jsengine.TrapError` (a failure of the module; the registry reports it as a panic). Never read error text. Only the C glue reads exception text, once, to tell an out-of-memory error from other exceptions.
  Why: the status codes of the ABI are the contract; message wording is not.
  Gate: `TestMemoryLimit_Honoured`, `TestCallExport_Deadline_IsErrCancelledAndVMStaysUsable`, `TestEval_Deadline_IsErrCancelled`, `TestHost_PanicIsTrapAndBreaksVM`, `TestRegistry_Invoke_ClassifiesOutcomes`.
- **R5** Every VM has a memory limit: `JS_SetMemoryLimit` from `spec.limits.memoryMB` for the QuickJS heap, and the wazero option `WithMemoryLimitPages` as one process-wide hard cap (`jsengine.MaxMemoryMB` plus headroom) below which the per-VM limit works. A script that hits the limit gets `ErrOOM`, and the VM stays usable.
  Why: one script must not exhaust the memory of the operator.
  Gate: `TestMemoryLimit_Honoured`, `TestMemoryLimit_GrowingHeapEndsInErrOOM`, `TestDefaultLimits_MatchKubebuilderTags`.
- **R6** Admission VMs get only read-only `kube` access.
  Why: an admission policy decides on a request; it must not change the
  cluster while it does so. The wiring check is missing (GATE-7).
  Gate: `TestSharedFactory_ForAdmission_ReadOnlySurface`, `TestReadOnlyKubeHost_ScriptSeesOnlyGetAndList`.
- **R7** Use a VM in one call only: restore it from the snapshot, run one export, close it when the call ends. Never close a VM while a call on it runs.
  Why: a VM instance is not goroutine-safe, and closing during a call races inside wazero.
  Gate: `TestSnapshot_ConcurrentRestores`, `TestRegistry_Concurrent_CallsBuildsAndDrop`.
- **R8** Never share one VM between two resources.
  Why: isolation between scripts depends on it.
  Gate: `TestVMs_AreIsolated`, `TestRegistry_SameNameInBothKindsCoexists`.
- **R9** (withdrawn, EXEC-8) Never write a field of `qjs.Context` after `jsengine.New`.
  Why: it existed for `fastschema/qjs`, which kept one library context for the life of the runtime; the own engine shares no context between calls (the per-call context travels as the context of each wasm call).
  Gate: `make test`.
- **R10** Reach script execution only through the port: `jsrun.Runner` (data path: dispatcher, admission server, `Handle` wrappers) or `jsrun.Scripts` (lifecycle: controllers). The dispatcher, the admission server, the controllers and `jslifecycle` (packages under `internal/jshook`, `internal/jsadmission`, `internal/jslifecycle`, `internal/conditions`) import none of `internal/jsengine`, wazero or `internal/jsregistry`; only `cmd` builds the adapter.
  Why: callers that hold a VM, a lock or the engine tie the operator to one engine and one execution model (persistent VM per key); behind the port a second engine (prepare once, invoke statelessly) can replace it.
  The data path packages (everything under `internal/jshook` and `internal/jsadmission` except the `controller` packages) name no `jsrun.Scripts`, `Spec`, `State` or `Phase*`.
  Gate: `TestImportBoundary_CallersUseOnlyRunnerPort`, `TestImportBoundary_DataPathUsesOnlyRunnerSide`.
- **R11** Give a script host functions only through `jsengine.Host.Func`, which crosses into wasm through the one import `env.host_call(name, json) -> json`. The Go function gets the context of the running call and JSON in and out; an error becomes a catchable exception with its message; a name that was not registered does not exist for the script, and the import itself is not reachable from it.
  Why: one narrow border to audit; a call that reaches the cluster (`kube.*`) ends with the deadline of the call (kube-access.R5); the read-only admission surface is just "not registered".
  Gate: `TestHost_CallsGoWithJSONAndReturnsJSON`, `TestHost_ErrorBecomesCatchableException`, `TestHost_OnlyRegisteredNamesExist`, `TestHost_FuncSeesCallContext`, `TestHost_DeadlineAfterCaughtHostError`.
- **R12** Offer the operator flag `--engine-cache-dir` for the wazero compilation cache. Empty (the default) keeps the compiled machine code in memory; a directory keeps it across restarts, so the first VM after a start takes about 15 ms instead of about 320 ms. Point it at a directory only the operator can write (for example an `emptyDir`): wazero runs the machine code it finds there. `cmd` compiles the engine at start-up (`jsregistry.ConfigureEngine`), so a bad directory stops the operator at once.
  Why: a restart of the operator rebuilds every VM; the compile of 1 MB of wasm would delay the first one.
  Gate: `TestFlags_EngineCacheDir_DefaultsToInMemory`, `TestCompilationCache_DirIsFilledAndReused`.
- **R13** Stop a running call through the QuickJS interrupt handler: the host import `env.interrupt` answers from `ctx.Err()` of the call. Do not use the wazero option `CloseOnContextDone`. A call that the deadline or a cancelled context stopped ends with `jsengine.ErrCancelled` (wrapping the context error); the VM keeps its state, so the engine itself never needs a rebuild after a timeout or the memory limit. A context that is over before the call starts never runs the script.
  Why: `CloseOnContextDone` kills the module and costs 4x to 7x on warm calls and loops (spike numbers in Decisions); the interrupt costs nothing measurable.
  Gate: `TestCallExport_Deadline_IsErrCancelledAndVMStaysUsable`, `TestDeadline_CannotBeCaught`, `TestCall_ContextAlreadyDone_DoesNotRun`, `TestDeadline_StopsCatastrophicRegex`, `TestRegistry_CancelledCall_NextCallGetsFreshInstance`; the cost guard is `BenchmarkVM_WarmCall` and `BenchmarkVM_Timeout`.
- **R14** Build `internal/jsengine/engine.wasm` only with `make engine-wasm`, from `glue/glue.c`, with the toolchain pinned in `internal/jsengine/glue/versions.env` (QuickJS-ng, wasi-sdk, binaryen; downloaded into `./bin`), and commit it with the change to `glue.c`.
  Why: the embedded binary must be reproducible from the repository; versions live in one file.
  Gate: missing → GATE-27.
- **R15** An error from a script carries its message, its stack with file and line, and the name of the export: `calling handle(): TypeError: x\n    at handle (hook.js:3:9)`. `*jsengine.JSError` holds `Message` and `Stack`; `Error()` has both.
  Why: an author must find the failing line from a log, an event or a denial message.
  Gate: `TestScriptError_CarriesMessageStackFileLine`, `TestScriptError_NonErrorThrow`.
- **R16** Restore a VM only through `jsengine.Snapshot.NewVM` from a snapshot that `jsengine.VM.Snapshot` took: a new instance without its start function, memory grown to the size of the snapshot, only the pages that differ from a fresh instance written back, the host binder and the limits of the snapshot set again, then `gj_update_stack_top` and `gj_reseed` with a new random seed. `Math.random` is our own xorshift64* in `glue.c`, seeded per VM.
  Why: a restore must cost a fraction of a millisecond and the same memory must not repeat in every snapshot (a sparse snapshot of a small policy is tens of KB, not the whole heap); without a reseed every call of a script would see the same random numbers. The QuickJS atom hash seed stays the one of the snapshot, which only shapes hash tables.
  Gate: `TestSnapshot_EveryVMStartsFromTheSameState`, `TestSnapshot_KeepsLimitsAndHost`, `TestSnapshot_ReseedsMathRandom`, `TestSnapshot_ConcurrentRestores`, `TestSnapshot_IsSparse`; the cost guard is `BenchmarkSnapshot_Shot`.
- **R17** Every VM gets `globalThis.console` (`log`, `info`, `debug`, `warn`, `error`), bound by `jslog.Binder` next to the `kube` surface. The lines go to the `jslog.Sink` in the context of the running call, so one prepared script serves calls of different callers. A call whose caller installed no sink drops the lines and runs on. Callers send every line to the operator log at `V(1)`; `warn` and `error` also become an Event with reason `ScriptMessage` on the hook or policy that wrote them, capped at `jslog.MaxTextBytes` per line and `jslog.MaxVisibleEvents` per call. That Message is the one exception to the low-cardinality rule for Event messages (status-conditions): it is written by the author of the script, for the person who deployed it.
  Why: without it a script has no voice at all — the only other globals are `kube.*`, so an author can neither trace their own code nor explain a failure to the person who deployed the hook, who usually cannot read the operator log.
  Gate: `TestConsole_EveryLevelReachesTheSink`, `TestConsole_JoinsEveryArgument`, `TestConsole_LogsAnErrorReadably`, `TestConsole_CapsOneLine`, `TestConsole_WithoutSink_DoesNotFailTheScript`, `TestConsole_RawHostFunctionIsHidden`, `TestSharedFactory_BothSurfaces_BindTheConsole`; the routing to log and Event: `TestDispatcher_ConsoleWarning_BecomesAnEvent`.

## Decisions

- **Use QuickJS on wazero as the engine.** Status: accepted.
  Why: no cgo, per-runtime isolation, cancellation by context. The reason it won over a native Go engine such as goja is not recorded.
  Not taken: goja, because the reason is not recorded.
- **Cancel calls with the wazero option `CloseOnContextDone`.** Status: superseded (2026-10-01, by "Cancel calls with the QuickJS interrupt handler" below).
  Why it was taken: the QuickJS execution time limit did nothing in `fastschema/qjs` v0.0.6.
- **Own QuickJS-ng wasm build instead of `fastschema/qjs`.** Status: proposed (2026-10-01, open) (EXEC-8).
  Why: `fastschema/qjs` (v0.0.6) is unmaintained, its execution time limit does nothing, and it converts every value field by field through many wasm calls. Our build (`glue.c`, QuickJS-ng v0.17.0, wasi-sdk 34, 0.95 MB) has a JSON ABI. Spike numbers (Apple M1 Pro, Go 1.26.1, wazero 1.9.0, a pod admission request of 4 KB): a warm call on a long-lived VM takes 65 µs with 14 allocations instead of 1.04 ms with 5727 (about 16x); building a VM, loading the sample policy and calling `validate` once takes 0.47 ms instead of 5.41 ms, and with the 544 KB lodash policy 21.7 ms instead of 107.2 ms. After the port, a warm call of the benchmark policy is 11 µs (`BenchmarkVM_WarmCall`).
  Not taken: staying on `fastschema/qjs`, because it blocks the interrupt handler and costs the speed above. A fork of `fastschema/qjs`, because it would keep the field-by-field conversion and make us maintain a binding we do not use.
- **Cancel calls with the QuickJS interrupt handler, not with `CloseOnContextDone`.** Status: proposed (2026-10-01, open) (EXEC-8; replaces the accepted decision "Cancel calls with the wazero option `CloseOnContextDone`", closes EXEC-3).
  Why: spike numbers, same engine with the option off and on: a warm `validate` on the large policy 81 µs against 304 µs (3.8x), a busy loop of 2M iterations 140 ms against 996 ms (7.1x), eval of lodash 22 ms against 114 ms (5.1x). With a deadline and the interrupt handler the busy loop stays at 136 ms. The module survives a timeout, so a timeout needs no rebuild.
  Not taken: `CloseOnContextDone`, as above. A watchdog goroutine that closes the module, because it kills the VM like `CloseOnContextDone` does.
- **Run every call single shot from a snapshot.** Status: proposed (2026-10-01, open) (EXEC-9).
  Why: every call starts from the same prepared state, so calls of one key run in parallel and a failed call leaves nothing behind; a call costs about 0.24 ms (`BenchmarkSnapshot_Shot`, 2.8 MB allocated). Scripts keep no top-level state between calls. Details and the semaphore: js-registry Decisions.
  Not taken: patching QuickJS to reseed `Math.random`, because an override in `glue.c` does it without carrying a patch. A full memory copy per snapshot, because the sparse one is much smaller.
- **Keep the compiled machine code in a wazero compilation cache with an optional directory.** Status: proposed (2026-10-01, open) (EXEC-8).
  Why: the first VM of a process costs 318 ms without a cache and 13 ms with a warm `wazero.NewCompilationCacheWithDir`; the operator would pay it at every restart.
  Not taken: the wazero interpreter, because every call is much slower (31 ms for the first VM, but slow after).

- **Split the port into `jsrun.Runner` and `jsrun.Scripts`.** Status: proposed (2026-10-01, open).
  Why: callers depend only on the half they use, and the port carries no persistent-VM words, so a stateless engine fits behind it. Details and what was removed (`Restart`, `Instance`, `Known`): js-registry Decisions.
  Not taken: one wide `Runner`, because every caller then depends on lifecycle methods it must not call.

## Open

GATE-7
GATE-27
