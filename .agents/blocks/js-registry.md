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

This block describes how gojsop keeps, builds, calls and restarts the JavaScript
VMs of JSHooks and JSAdmissions.

One `jsregistry.Registry` exists per process. It holds one long-lived VM per
JSHook or JSAdmission, keyed by `types.NamespacedName`. Controllers build,
restart and drop VMs. The dispatcher and the admission server only call them.
All runtime execution goes through `Registry.Call`, which takes the per-VM
lock, recovers panics and classifies the outcome as ok, panic, cancelled,
memory limit or error. The [js-execution](js-execution.md) block covers the
engine below it.

The registry exists because the dispatcher and the admission server each had
their own copy of lock, recover and error classification. `Registry.Call` unified
them. A build runs user JavaScript, so it holds a per-key lock and never the
registry lock; a slow build of one key must not block other keys. The commit
history gives no further reason for this split. Per-reason restart counters and
the history ring on the VM were added for observability; no deeper reason is
recorded. Rejected alternatives are not recorded anywhere.

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

## Rules

- **R1** Run user JavaScript at runtime only in the function passed to `Registry.Call`.
  Why: one place for the call lock, panic recovery and outcome classification.
  Gate: missing → GATE-4. See also js-execution R2.
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

## Open

Tracked in [backlog](../backlog.md): REG-1 to REG-7; gates GATE-2, GATE-3, GATE-4.
