---
id: js-execution
status: accepted
entrypoints:
  - jsengine.New
  - jsengine.VM.CallExport
  - jsengine.VM.WithContext
  - jsregistry.Registry.Call
---

# JS Execution

This aspect describes how gojsop executes user JavaScript inside the cluster.

Every JSHook and every JSAdmission gets its own JavaScript runtime. The runtime
is QuickJS, compiled to WebAssembly and run by wazero inside the operator
process. The Go binding is the library `fastschema/qjs`. Three properties
follow from this choice:

- **No cgo.** The operator builds with `CGO_ENABLED=0`. wazero runs
  WebAssembly in pure Go, so the engine needs no C toolchain and no native
  library in the image.
- **Isolation.** Each runtime is a separate WebAssembly instance with its own
  heap. One script cannot read or damage the memory of another, and the memory
  limit applies to each script on its own.
- **Hard cancellation.** wazero can stop running WebAssembly code from the
  outside when a Go context ends. A script in an endless loop does not block
  the operator.

Why QuickJS on wazero won over a native Go engine such as goja is not
recorded.

Runtimes are long-lived. The top-level state of a script survives between
calls, so a hook can keep caches. The price is that a broken runtime must be
found and rebuilt. The [js-registry](js-registry.md) aspect covers that.

A call ends when its context ends. wazero closes the module then, so a
cancelled VM is dead and its owner must rebuild it; closing it does not panic. The execution time limit of QuickJS does
nothing in qjs v0.0.6, so the engine uses the wazero option
`CloseOnContextDone`. When the deadline of the caller passes, wazero aborts
the running code and the call returns `jsengine.ErrCancelled`. The deadline
comes from `spec.limits.timeoutSeconds`, the heap limit from
`spec.limits.memoryMB`. The defaults are 30 s and 32 MB (`jsengine.DefaultLimits`).

Scripts reach the outside world only through the global object `kube`. Hooks
get `apply`, `get`, `list` and `delete`. Admission policies get read-only
`get` and `list`. Nothing else is bound: no network, no file system, no
timers. The [kube-access](kube-access.md) aspect covers the details.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once? | `jsengine.New` and `jsengine.VM` (the only wrapper of `fastschema/qjs` and wazero), run through `jsregistry.Registry.Call`. A second way exists: `jsadmission` imports `fastschema/qjs` in its result decoding (EXEC-1). |
| example | Which real use should others copy? | `jsregistry.Registry.Call`, used by the JSHook and JSAdmission handlers. |
| test helper | How does a test use the aspect without effort? | `jsengine.New` with `jsengine.Limits{}` builds a real VM in a test; `kubehost` tests use the helper `newKubeHost`. No shared harness package (searched `_test.go` for helpers and fakes). |
| sides | Which sides does it touch? | n/a, because it is one side: the operator process (Go). Searched the repository for other languages and binaries. The CRD defaults are the only outside edge. |
| tie | How do the sides stay in step? | The CRD defaults for `spec.limits` and `jsengine.DefaultLimits` are tied by `TestDefaultLimits_MatchKubebuilderTags`. |

## How to use it

Add a new kind of resource that runs JavaScript:

1. Build its VM only through `jsregistry.Registry.GetOrLoad` with explicit `jsengine.Limits`.
2. Run its script only inside `jsregistry.Registry.Call`, with a deadline from `spec.limits.timeoutSeconds` (R2, R3).
3. Classify errors with `errors.Is` against `jsengine.ErrCancelled` and `jsengine.ErrOOM` (R4).
4. Choose the `kube` surface from `kubehost.Factory`: read-only for decisions (R6).
5. Gate: `TestMemoryLimit_Honoured` and `TestSharedFactory_ForAdmission_ReadOnlySurface`.

## Rules

- **R1** Only package `jsengine` imports `fastschema/qjs` or wazero.
  Why: the engine stays replaceable, and error handling stays in one place. Broken today by `jsadmission` (EXEC-1).
  Gate: missing → GATE-2.
- **R2** Run user JavaScript only inside `jsregistry.Registry.Call`. The only
  exception is building a VM inside the registry (module load and `config()`).
  Why: one place for the call lock, panic recovery and result classification.
  Gate: missing → GATE-4.
- **R3** Give every call a context with a deadline from
  `spec.limits.timeoutSeconds`.
  Why: without a deadline an endless loop holds the VM forever.
  Gate: missing → GATE-5.
- **R4** Classify engine errors with `errors.Is` against `jsengine.ErrCancelled`
  and `jsengine.ErrOOM`. Only `jsengine.wrapEngineErr` and `jsengine.closedModulePanic` (the one qjs panic the engine converts: a call on a module wazero closed) may inspect error or panic text.
  Why: qjs and wazero error strings change between versions.
  Gate: `TestWrapOOM`, `TestIsOOMError_OnlyMatchesSentinel`, `TestCallExport_Deadline_IsErrCancelledNotPanic`, `TestEval_Deadline_IsErrCancelledNotPanic`.
- **R5** Every VM has a memory limit.
  Why: one script must not exhaust the memory of the operator.
  Gate: `TestMemoryLimit_Honoured`, `TestDefaultLimits_MatchKubebuilderTags`.
- **R6** Admission VMs get only read-only `kube` access.
  Why: an admission policy decides on a request; it must not change the
  cluster while it does so. The wiring check is missing (GATE-7).
  Gate: `TestSharedFactory_ForAdmission_ReadOnlySurface`.
- **R7** Close a VM only while you hold its call lock.
  Why: closing during a running call races inside wazero.
  Gate: `TestRegistry_Concurrent_CallRestartDrop`.
- **R8** Never share one VM between two resources.
  Why: isolation between scripts depends on it.
  Gate: missing → GATE-6.
- **R9** Never write a field of `qjs.Context` after `jsengine.New`. The per-call
  context goes through `jsengine.VM.withContext`, which moves it behind an
  atomic pointer in a fixed delegating context.
  Why: wazero starts a goroutine per wasm call that reads the embedded context
  later, so a write races with it.
  Gate: `TestVM_SequentialCalls_NoContextRace` (needs `go test -race`, which `make test` runs).

## Decisions

- **Use QuickJS on wazero (via `fastschema/qjs`) as the engine.** Status: accepted.
  Why: no cgo, per-runtime isolation, hard cancellation by context. The reason it won over a native Go engine such as goja is not recorded.
  Not taken: goja, because the reason is not recorded.
- **Cancel calls with the wazero option `CloseOnContextDone`.** Status: accepted.
  Why: the QuickJS execution time limit does nothing in qjs v0.0.6.
  Not taken: fork qjs to get QuickJS interrupts. Kept as a fallback if the cancellation overhead turns out too high (EXEC-3).

## Open

EXEC-1
EXEC-2
EXEC-3
EXEC-4
GATE-2
GATE-4
GATE-5
GATE-6
GATE-7
