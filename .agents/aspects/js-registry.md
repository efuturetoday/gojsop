---
id: js-registry
status: accepted
entrypoints:
  - jsrun.Runner.Invoke
  - jsrun.Runner.Ensure
  - jsrun.Runner.Watch
  - jsrun.Runner.Restart
  - jsregistry.Registry.Call
  - jslifecycle.Rescue
---

# JS Registry

This aspect describes how gojsop keeps, builds, calls and restarts the JavaScript
VMs of JSHooks and JSAdmissions.

Callers talk to the port `jsrun.Runner`, never to the registry or the engine:
they hand in a key, an export name and an input and get an output and a
classified `jsrun.Result`. The first adapter of the port is
`jsregistry.Registry`. One exists per process. It holds one long-lived VM per
JSHook or JSAdmission, keyed by `jsrun.Key` (kind and name). Controllers build,
restart and drop VMs. The dispatcher and the admission server only call them.
All runtime execution goes through `Runner.Invoke`, which the registry
implements with `Registry.Call`: it takes the per-VM
lock, recovers panics and classifies the outcome as ok, panic, cancelled,
memory limit or error. The port also fits an engine that prepares a script once
and invokes it statelessly: `Ensure` is the prepare, `Invoke` the run. The [js-execution](js-execution.md) aspect covers the
engine below it.

The registry exists because the dispatcher and the admission server each had
their own copy of lock, recover and error classification. `Registry.Call` unified
them. Per-reason restart counters and the history ring on the VM were added for
observability. See Decisions for the choices behind the build lock.

The registry never loads sources. A controller loads the bytes, hashes them and
passes both in `jsrun.Options`. A changed hash or changed effective limits make `Ensure` start a rebuild. The
registry caches the whole `jsrun.Options`, so `Runner.Restart` can rebuild without
the controller. This is how a rescue works from the dispatcher or the admission
server, which hold no source.

A restart has one of six reasons: source changed, limits changed, memory limit,
panic, timeout or manual (the CRD enum still lists `timeout-streak`, which nothing sets: a
cancelled call closes the module, so every timeout restarts, REG-4). `jslifecycle.Rescue` wraps `Runner.Restart` and emits
the `Restarted` and `RescueFailed` events. A restart does not build in the caller: it closes the VM and starts the same background build as `Ensure`, so until it ends the key holds no VM and `Runner.Invoke` returns `ErrVMUnavailable`. Per-VM data that a controller needs
(for example the parsed JSHook config) is computed in a `PostBuildHook` and
stored in `Instance.Extra`. The same hook checks required exports. The hook sees the new script only as a `jsrun.Script`, never as a VM.

JSHook calls arrive from an informer queue and a FIFO worker. JSAdmission
calls arrive from a synchronous webhook. Both share one per-VM lock.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once, so nobody builds it a second time? | The port `jsrun.Runner` (`jsrun.Runner.Ensure`, `jsrun.Runner.Invoke`, `jsrun.Runner.Restart`, `jsrun.Runner.Watch`); its adapter `jsregistry.Registry` with `jsregistry.Registry.Call`; `jslifecycle.Rescue` for the restart path. Searched `internal` for a second lock, recover or restart path around it; the dispatcher and the admission server call the port and have none. |
| example | Which real use in the code should others copy? | `controller.JSHookReconciler` reacts to `Runner.Ensure` (Building, Broken, Ready) with a `PostBuildHook`; `jshook/dispatcher` calls `jshook.Handle` (a `Runner.Invoke`) and `jslifecycle.Rescue`. |
| test helper | How does a test use the aspect without effort? | `jsregistry.NewRegistry()` is cheap and needs no cluster and is a `jsrun.Runner`; tests build a VM with `registrytest.GetOrLoad`, which polls `Ensure`, and a `jsrun.Options` literal. `loadPolicy` in `internal/jsadmission/server_test.go` is a local helper for admission tests; no fake of the port exists, because the real registry is cheap. |
| sides | Which sides does it touch? | Back end only: the operator process (controllers, dispatcher, admission server). No front end, no CLI. |
| tie | If it touches more than one side: how do the sides stay in step? | n/a, because it touches one side. Searched `cmd`, `internal` and `test` for other users of the port; all are in the operator process. |

## How to use it

1. In a controller, load the source and compute its hash, then call `Runner.Ensure` with `jsrun.Options` (source, hash, limits, host, `PostBuildHook`, backoff). Building: write `Ready=False`/`Building` and return; the runner notifies the controller through `Runner.Watch` when the build ends. Broken: write `Ready=False`/`BuildFailed` and requeue after the backoff. Ready: continue.
2. Compute per-VM data in the `PostBuildHook` and read it from `Instance.Extra`.
3. Run JavaScript only through `Runner.Invoke` (typed wrappers: `jshook.Handle`, `jsadmission.Handle`); on panic, memory limit or timeout call `jslifecycle.Rescue`, which starts the rebuild and returns; handle `ErrVMUnavailable` from `Runner.Invoke` by retrying later (events) or by your failure policy (admission).
4. Drop the VM with `Runner.Drop` when the resource goes away.
5. Add a test with `jsregistry.NewRegistry()` (or a `jsrun.Runner` of your own) and carry the rule ID in a comment above it.

## Rules

- **R1** Run user JavaScript at runtime only through `Runner.Invoke` (the registry runs it in the function passed to `Registry.Call`).
  Why: one place for the call lock, panic recovery and outcome classification.
  Gate: missing → GATE-4. See also js-execution.R2.
- **R2** Call `jslifecycle.Rescue` when `Runner.Invoke` reports panic, memory limit or timeout.
  Why: a VM in that state is not trusted; restarting it is the recovery path.
  Gate: `TestRescue_Success_EmitsRestarted`, `TestRescue_Failure_EmitsRescueFailed`, `TestRescue_NilEmitter_NoOps`.
- **R3** Resolve and hash the source in the controller, never in the registry.
  Why: the registry stays independent of source loading; the hash drives rebuilds.
  Gate: `TestRegistry_RestartOnSourceChange`.
- **R4** Rebuild a restarted VM from its cached `jsrun.Options`.
  Why: rescue callers hold no source, limits or host binder.
  Gate: `TestRegistry_RestartByKey_RebuildsFromCachedSource`, `TestRegistry_RestartByKey_UnknownHook`, `TestRegistry_CancelledCall_IsCancelledAndRestartRebuildsDeadVM`.
- **R5** Keep one VM per key across reconciles while the source hash is unchanged.
  Why: scripts keep top-level state between calls (see js-execution).
  Gate: `TestRegistry_PersistsAcrossLoads`.
- **R6** Hold the registry lock only for map access, never while user JavaScript runs.
  Why: a slow build or call of one key must not block other keys.
  Gate: `TestRegistry_Concurrent_CallRestartDrop`.
- **R7** Serialise builds and restarts per key with the build lock.
  Why: two reconciles of one key must not race on construction.
  Gate: `TestRegistry_Concurrent_CallRestartDrop`.
- **R8** Take the old VM's `ManagedVM.CallMu` before closing or replacing it.
  Why: the qjs runtime is not goroutine-safe, and closing during a call races in wazero.
  Gate: `TestRegistry_Concurrent_CallRestartDrop`.
- **R9** Touch `ManagedVM.VM` and `ManagedVM.CallMu` only inside `internal/jsregistry`; callers see only `jsrun.Instance`.
  Why: the fields are exported, but the lock protocol lives in the registry. Callers cannot reach them, because they may not import the registry (js-execution.R10). Tests and `cmd` may.
  Gate: `TestImportBoundary_CallersUseOnlyRunnerPort`.
- **R10** Cache per-VM data in `Instance.Extra` through `PostBuildHook`, not by calling the VM from a reconcile.
  Why: reconciles must not run user JavaScript outside the build.
  Gate: review only — a convention about where state lives.
- **R11** Record every restart with its reason in the per-reason counters and the history ring.
  Why: operators need to see why a VM restarted.
  Gate: `TestRegistry_RestartHistory_RingAndCounters`.
- **R12** Rebuild a VM when `Ensure` gets other effective limits, even with an unchanged source hash.
  Why: a changed `spec.limits` must take effect; zero fields count as the defaults, so an explicit default is no change. The restart reason is `limits-changed`.
  Gate: `TestRegistry_Ensure_RebuildsOnLimitsChange`.
- **R13** Bound every build, the rescue build of `Runner.Restart` too, by the VM's timeout limit.
  Why: a hanging module or `PostBuild` must not hold the per-key build lock forever.
  Gate: `TestRegistry_RestartByKey_BuildHasDeadlineAndFailureHoldsNoVM`, `TestRegistry_Ensure_HangingBuildDoesNotBlockOtherKeys`.
- **R14** Key every registry entry by `jsrun.Key`, which carries the kind next to the name.
  Why: both kinds are cluster-scoped and share one registry, so a JSHook and a JSAdmission of the same name must not replace each other's VM.
  Gate: `TestRegistry_SameNameInBothKindsCoexists`.
- **R15** Never wait for a build in a reconcile or a call: ask `Runner.Ensure`, which reports Ready, Building or Broken at once.
  Why: a hanging top-level `while(true){}` must not stall the reconcile worker of other hooks.
  Gate: `TestRegistry_Ensure_BuildsAsyncAndNotifies`, `TestRegistry_Ensure_HangingBuildDoesNotBlockOtherKeys`, `TestReconcile_HangingBuildDoesNotBlockOtherHook`.
- **R16** Cancel the running build of a key through its context when `Ensure` gets other options, and start the new build.
  Why: a fixed source must not wait for the stuck one to run into its deadline.
  Gate: `TestRegistry_Ensure_NewOptsCancelRunningBuild`, `TestRegistry_Concurrent_EnsureCallDrop`.
- **R17** Run one build/retry loop per key: a Broken key is retried only after its backoff, changed options rebuild at once.
  Why: a bad source must not be rebuilt in a tight loop, and a fix must not wait for the backoff.
  Gate: `TestRegistry_Ensure_BrokenBacksOffAndSourceChangeRebuildsAtOnce`, `TestBackoff_DoublesCapsAndJitters`, `TestReconcile_BrokenBuild_BacksOffAndSourceChangeRebuildsAtOnce`, `TestFlags_BuildBackoffReachesReconcilers`, `TestFlags_BuildBackoffRejectsNonsense`.
  Base and cap are the operator flags `--build-backoff-base` (1s) and `--build-backoff-max` (5m); both reconcilers hand them to `Ensure`.
- **R18** Notify the key on the `Runner.Watch` channel of its kind when a build ends, whether it installed a VM or failed.
  Why: reconciles do not wait, so the controller of the kind needs the event to publish the new state.
  Gate: `TestRegistry_Ensure_BuildsAsyncAndNotifies`, `TestRegistry_Watch_DeliversOnlyOwnKind`.
- **R19** Hold no VM in a Broken entry or in one that a call or a manual restart sent to rebuild, and let `Runner.Invoke` return `ErrVMUnavailable` at once for it. A build for changed options keeps the old VM serving until the new one is installed; every failed build drops the old VM.
  Why: a VM after a timeout is dead, and calls on it panic and trigger one rescue each (a loop of rescues); callers must see "not ready" and apply their own policy (requeue, `failurePolicy`).
  Gate: `TestRegistry_Call_WithoutVM_IsErrVMUnavailable`, `TestRegistry_RestartByKey_BuildHasDeadlineAndFailureHoldsNoVM`.

## Decisions

- **A build runs user JavaScript under a per-key lock and never under the registry lock.** Status: accepted (carried over from the block, no date or name recorded).
  Why: a slow build of one key must not block other keys. Not taken: not recorded anywhere.
- **`Runner.Invoke` (`Registry.Call` in the adapter) is the one place for lock, panic recovery and outcome classification.** Status: accepted (carried over from the block, no date or name recorded).
  Why: the dispatcher and the admission server each had a copy. Not taken: not recorded anywhere.
- **The registry caches the whole `jsrun.Options` and never loads sources.** Status: accepted (carried over from the block, no date or name recorded).
  Why: rescue callers hold no source. Not taken: not recorded anywhere.

- **Builds run asynchronously; a newer build cancels a running one.** Status: proposed.
  Why: a build runs user JavaScript that may hang; a reconcile or call that waits for it stalls other hooks, and a stuck build blocks the fix. Every key is in one state, Ready, Building or Broken, and `Runner.Ensure` reports it without waiting.
  A restart after a call (panic, memory limit, timeout) or a manual restart closes the VM and builds in the background too; a failed build leaves the key Broken without a VM, so no dead VM serves calls and no call triggers a rescue. Callers meet `ErrVMUnavailable`: the dispatcher requeues the event, the admission server applies `failurePolicy`.
  Not taken: a synchronous build with a reconcile deadline, because the build lock stays held by the hanging build and the reconcile worker still waits. Keeping the old VM after a failed rescue build, because after a timeout it is dead.

- **Callers depend on the port `jsrun.Runner`, and the registry is its first adapter.** Status: accepted (2026-10, project, EXEC-6).
  Why: the dispatcher, the admission server, the controllers and `jslifecycle` named `jsregistry.Registry` and `jsengine.VM`, so no other engine could run behind them. The spike `spike/quickjs-wasm` shows an engine that runs single-shot calls from a snapshot next to long-lived VMs; the port (ensure, invoke by key and export, restart, drop, watch) fits both and hides VMs, locks and the engine.
  Not taken: a `Call(fn(vm))` callback in the port, because it hands the engine to the caller. A fake of the port for caller tests, because the real registry is cheap and a fake would repeat its states.

## Open

Tracked in [backlog](../backlog.md): REG-3 to REG-6; gate GATE-4.
