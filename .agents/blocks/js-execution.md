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

This block describes how gojsop executes user JavaScript inside the cluster.

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
found and rebuilt. The [js-registry](js-registry.md) block covers that.

A call ends when its context ends. The execution time limit of QuickJS does
nothing in qjs v0.0.6, so the engine uses the wazero option
`CloseOnContextDone`. When the deadline of the caller passes, wazero aborts
the running code and the call returns `jsengine.ErrCancelled`. The deadline
comes from `spec.limits.timeoutSeconds`, the heap limit from
`spec.limits.memoryMB`. The defaults are 30 s and 32 MB (`jsengine.DefaultLimits`).

Scripts reach the outside world only through the global object `kube`. Hooks
get `apply`, `get`, `list` and `delete`. Admission policies get read-only
`get` and `list`. Nothing else is bound: no network, no file system, no
timers. The [kube-access](kube-access.md) block covers the details.

## Rules

- **R1** Only package `jsengine` imports `fastschema/qjs` or wazero.
  Why: the engine stays replaceable, and error handling stays in one place.
  Gate: missing → GATE-2. Violated today → EXEC-1.
- **R2** Run user JavaScript only inside `jsregistry.Registry.Call`. The only
  exception is building a VM inside the registry (module load and `config()`).
  Why: one place for the call lock, panic recovery and result classification.
  Gate: missing → GATE-4.
- **R3** Give every call a context with a deadline from
  `spec.limits.timeoutSeconds`.
  Why: without a deadline an endless loop holds the VM forever.
  Gate: missing → GATE-5.
- **R4** Classify engine errors with `errors.Is` against `jsengine.ErrCancelled`
  and `jsengine.ErrOOM`. Only `jsengine.wrapEngineErr` may inspect error text.
  Why: qjs and wazero error strings change between versions.
  Gate: `TestWrapOOM`, `TestIsOOMError_OnlyMatchesSentinel`.
- **R5** Every VM has a memory limit.
  Why: one script must not exhaust the memory of the operator.
  Gate: `TestMemoryLimit_Honoured`, `TestDefaultLimits_MatchKubebuilderTags`.
- **R6** Admission VMs get only read-only `kube` access.
  Why: an admission policy decides on a request; it must not change the
  cluster while it does so.
  Gate: `TestSharedFactory_ForAdmission_ReadOnlySurface`. Wiring check
  missing → GATE-7.
- **R7** Close a VM only while you hold its call lock.
  Why: closing during a running call races inside wazero.
  Gate: missing → GATE-3.
- **R8** Never share one VM between two resources.
  Why: isolation between scripts depends on it.
  Gate: missing → GATE-6.

## Rejected

- Fork qjs to get QuickJS interrupts instead of `CloseOnContextDone`. Kept as
  a fallback if the cancellation overhead turns out too high (EXEC-3).

## Open

Tracked in [backlog](../backlog.md): EXEC-1 to EXEC-4; gates GATE-2, GATE-3,
GATE-5, GATE-6, GATE-7.
