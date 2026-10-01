---
id: js-registry
status: accepted
entrypoints:
  - jsregistry.Registry.Call
  - jsregistry.Registry.GetOrLoad
  - jsregistry.Registry.RestartByKey
  - jslifecycle.Rescue
---

# JS Registry

This aspect describes how gojsop keeps, builds, calls and restarts the JavaScript
VMs of JSHooks and JSAdmissions.

One `jsregistry.Registry` exists per process. It holds one long-lived VM per
JSHook or JSAdmission, keyed by `types.NamespacedName`. Controllers build,
restart and drop VMs. The dispatcher and the admission server only call them.
All runtime execution goes through `Registry.Call`, which takes the per-VM
lock, recovers panics and classifies the outcome as ok, panic, cancelled,
memory limit or error. The [js-execution](js-execution.md) aspect covers the
engine below it.

The registry exists because the dispatcher and the admission server each had
their own copy of lock, recover and error classification. `Registry.Call` unified
them. Per-reason restart counters and the history ring on the VM were added for
observability. See Decisions for the choices behind the build lock.

The registry never loads sources. A controller loads the bytes, hashes them and
passes both in `BuildOptions`. A changed hash makes `GetOrLoad` rebuild. The
registry caches the whole `BuildOptions`, so `RestartByKey` can rebuild without
the controller. This is how a rescue works from the dispatcher or the admission
server, which hold no source.

A restart has one of six reasons: source changed, memory limit, panic, timeout,
timeout streak or manual. `jslifecycle.Rescue` wraps `RestartByKey` and emits
the `Restarted` and `RescueFailed` events. Per-VM data that a controller needs
(for example the parsed JSHook config) is computed in a `PostBuildHook` and
stored in `ManagedVM.Extra`. The same hook checks required exports.

JSHook calls arrive from an informer queue and a FIFO worker. JSAdmission
calls arrive from a synchronous webhook. Both share one per-VM lock.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once, so nobody builds it a second time? | `jsregistry.Registry` with `jsregistry.Registry.Call`, `jsregistry.Registry.GetOrLoad` and `jsregistry.Registry.RestartByKey`; `jslifecycle.Rescue` for the restart path. Searched `internal` for a second lock, recover or restart path around it; the dispatcher and the admission server call the registry and have none. |
| example | Which real use in the code should others copy? | `controller.JSHookReconciler` builds through `Registry.GetOrLoad` with a `PostBuildHook`; `jshook/dispatcher` calls `Registry.Call` and `jslifecycle.Rescue`. |
| test helper | How does a test use the aspect without effort? | `jsregistry.NewRegistry()` is cheap and needs no cluster; tests build a VM with `GetOrLoad` and a `BuildOptions` literal. `loadPolicy` in `internal/jsadmission/server_test.go` is a local helper for admission tests; no shared fake exists. |
| sides | Which sides does it touch? | Back end only: the operator process (controllers, dispatcher, admission server). No front end, no CLI. |
| tie | If it touches more than one side: how do the sides stay in step? | n/a, because it touches one side. Searched `cmd`, `internal` and `test` for other users of `jsregistry`; all are in the operator process. |

## How to use it

1. In a controller, load the source and compute its hash, then call `Registry.GetOrLoad` with `BuildOptions` (source, hash, limits, host binder, `PostBuildHook`).
2. Compute per-VM data in the `PostBuildHook` and read it from `ManagedVM.Extra`.
3. Run JavaScript only through `Registry.Call`; on panic, memory limit or timeout call `jslifecycle.Rescue`.
4. Drop the VM with `Registry.Drop` when the resource goes away.
5. Add a test with `jsregistry.NewRegistry()` and carry the rule ID in a comment above it.

## Rules

- **R1** Run user JavaScript at runtime only in the function passed to `Registry.Call`.
  Why: one place for the call lock, panic recovery and outcome classification.
  Gate: missing → GATE-4. See also js-execution.R2.
- **R2** Call `jslifecycle.Rescue` when `Registry.Call` reports panic, memory limit or timeout.
  Why: a VM in that state is not trusted; restarting it is the recovery path.
  Gate: `TestRescue_Success_EmitsRestarted`, `TestRescue_Failure_EmitsRescueFailed`, `TestRescue_NilEmitter_NoOps`.
- **R3** Resolve and hash the source in the controller, never in the registry.
  Why: the registry stays independent of source loading; the hash drives rebuilds.
  Gate: `TestRegistry_RestartOnSourceChange`.
- **R4** Rebuild a restarted VM from its cached `BuildOptions`.
  Why: rescue callers hold no source, limits or host binder.
  Gate: `TestRegistry_RestartByKey_RebuildsFromCachedSource`, `TestRegistry_RestartByKey_UnknownHook`.
- **R5** Keep one VM per key across reconciles while the source hash is unchanged.
  Why: scripts keep top-level state between calls (see js-execution).
  Gate: `TestRegistry_PersistsAcrossLoads`. Violated today → REG-7.
- **R6** Hold the registry lock only for map access, never while user JavaScript runs.
  Why: a slow build or call of one key must not block other keys.
  Gate: missing → GATE-3.
- **R7** Serialise builds and restarts per key with the build lock.
  Why: two reconciles of one key must not race on construction.
  Gate: missing → GATE-3.
- **R8** Take the old VM's `ManagedVM.CallMu` before closing or replacing it.
  Why: the qjs runtime is not goroutine-safe, and closing during a call races in wazero.
  Gate: missing → GATE-3.
- **R9** Touch `ManagedVM.VM` and `ManagedVM.CallMu` only inside `internal/jsregistry`.
  Why: the fields are exported, but the lock protocol lives in the registry.
  Gate: missing → GATE-2.
- **R10** Cache per-VM data in `ManagedVM.Extra` through `PostBuildHook`, not by calling the VM from a reconcile.
  Why: reconciles must not run user JavaScript outside the build.
  Gate: review only — a convention about where state lives.
- **R11** Record every restart with its reason in the per-reason counters and the history ring.
  Why: operators need to see why a VM restarted.
  Gate: `TestRegistry_RestartHistory_RingAndCounters`.

## Decisions

- **A build runs user JavaScript under a per-key lock and never under the registry lock.** Status: accepted (carried over from the block, no date or name recorded).
  Why: a slow build of one key must not block other keys. Not taken: not recorded anywhere.
- **`Registry.Call` is the one place for lock, panic recovery and outcome classification.** Status: accepted (carried over from the block, no date or name recorded).
  Why: the dispatcher and the admission server each had a copy. Not taken: not recorded anywhere.
- **The registry caches the whole `BuildOptions` and never loads sources.** Status: accepted (carried over from the block, no date or name recorded).
  Why: rescue callers hold no source. Not taken: not recorded anywhere.

## Open

Tracked in [backlog](../backlog.md): REG-1 to REG-7; gates GATE-2, GATE-3, GATE-4.
