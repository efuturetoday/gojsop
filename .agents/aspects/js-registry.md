---
id: js-registry
status: accepted
entrypoints:
  - jsrun.Runner.Invoke
  - jsrun.Scripts.Ensure
  - jsrun.Scripts.Drop
  - jsrun.Scripts.Watch
  - jsregistry.Registry.Call
---

# JS Registry

This aspect describes how gojsop prepares, keeps and calls the JavaScript of
JSHooks and JSAdmissions.

Callers talk to the port, never to the registry or the engine. The port has
two interfaces. `jsrun.Runner` is the data path: callers hand in a key, an
export name and an input and get an output and a classified `jsrun.Result`;
the dispatcher, the admission server and the `Handle` wrappers hold only this.
`jsrun.Scripts` is the lifecycle path: the controllers `Ensure` a script for a
`jsrun.Spec`, `Drop` it and `Watch` their kind. The first adapter of both is
`jsregistry.Registry`. One exists per process. It holds one prepared script
(`jsregistry.Prepared`) per JSHook or JSAdmission, keyed by `jsrun.Key` (kind
and name). A prepared script is a memory snapshot of a VM taken right after
the top-level code ran (`jsengine.Snapshot`). Controllers prepare and drop
scripts. The dispatcher and the admission server only call them.
All runtime execution goes through `Runner.Invoke`, which the registry
implements with `Registry.Call` (single shot): it waits for a slot of the
process-wide call semaphore, restores a fresh VM from the snapshot, bounds the
call by the timeout limit, recovers panics, classifies the outcome as ok,
panic, cancelled, memory limit or error (a wasm trap counts as a panic) and
closes the VM. Calls of one key run in parallel, each on its own VM; nothing
of a call survives into the next. `Ensure` is the prepare, `Invoke` the run. The port names no VM, restart or instance: `State`
(Phase Preparing, Ready or Failed, Err, Attempts, NextAttempt, PreparedAt, Meta,
Recoveries) is all a controller learns about a script. The [js-execution](js-execution.md) aspect covers the
engine below it.

`Registry.Call` is the one place for the call semaphore, panic recovery and
outcome classification; the dispatcher and the admission server have none of
their own. See Decisions for the choices behind the build lock and single
shot.

The registry never loads sources. A controller loads the bytes, hashes them and
passes both in `jsrun.Spec`. A changed hash, changed effective limits or a
changed `Spec.ResetToken` make `Ensure` start a rebuild in the background; the
old script keeps serving until the new one is installed. The registry caches
the whole `jsrun.Spec` of the installed script.

A call never recovers or rebuilds anything: a panic, a trap, a timeout or the
memory limit end that call with the matching outcome, its VM is thrown away,
and the next call starts fresh from the same snapshot. So a recovery (an entry
in `State.Recoveries`) comes only from `Ensure`, with one of three reasons:
source changed, limits changed or manual. The CRD enum also lists
`memory-limit`, `panic`, `timeout` and `timeout-streak`, which nothing sets
(REG-4). The manual restart annotation reaches the port as
`Spec.ResetToken`: a new value prepares again like a changed source, with
reason manual. No `Restart` exists on the port. Per-script data that a
controller needs (for example the parsed JSHook config) is computed in a
`PostBuildHook` and stored in `State.Meta`. The same hook checks required
exports. The hook sees the new script only as a `jsrun.Script`, never as a VM;
it runs on the VM the snapshot was taken from, after the snapshot, so what it
changes does not reach the calls.

JSHook calls arrive from an informer queue and a FIFO worker per
subscription. JSAdmission calls arrive from a synchronous webhook. Both share
the process-wide call semaphore (`--max-concurrent-calls`, default 16), which
bounds the memory all running calls take together.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once, so nobody builds it a second time? | The port with its two interfaces `jsrun.Runner` (`jsrun.Runner.Invoke`) and `jsrun.Scripts` (`jsrun.Scripts.Ensure`, `jsrun.Scripts.Drop`, `jsrun.Scripts.Watch`); its adapter `jsregistry.Registry` with `jsregistry.Registry.Call`, which restores a VM per call from the snapshot. Searched `internal` for a second semaphore, recover or restart path around it; the dispatcher and the admission server call the port and have none. |
| example | Which real use in the code should others copy? | `controller.JSHookReconciler` reacts to `Scripts.Ensure` (Preparing, Failed, Ready) with a `PostBuildHook`; `jshook/dispatcher` calls `jshook.Handle` (a `Runner.Invoke`) and dispatches on the outcome. |
| test helper | How does a test use the aspect without effort? | `jsregistry.NewRegistry()` is cheap and needs no cluster and is a `jsrun.Runner` and a `jsrun.Scripts`; tests build a VM with `registrytest.GetOrLoad`, which polls `Ensure`, and a `jsrun.Spec` literal; `Registry.State` reads a state without a spec. `loadPolicy` in `internal/jsadmission/server_test.go` is a local helper for admission tests; no fake of the port exists, because the real registry is cheap. |
| sides | Which sides does it touch? | Back end only: the operator process (controllers, dispatcher, admission server). No front end, no CLI. |
| tie | If it touches more than one side: how do the sides stay in step? | n/a, because it touches one side. Searched `cmd`, `internal` and `test` for other users of the port; all are in the operator process. |

## How to use it

1. In a controller, load the source and compute its hash, then call `Scripts.Ensure` with `jsrun.Spec` (source, hash, limits, host, `PostBuildHook`, `ResetToken`, backoff). Preparing: write `Ready=False`/`Building` and return; the runner notifies the controller through `Scripts.Watch` when the build ends. Failed: write `Ready=False`/`BuildFailed` and requeue after the backoff. Ready: continue.
2. Compute per-script data in the `PostBuildHook` and read it from `State.Meta`.
3. Run JavaScript only through `Runner.Invoke` (typed wrappers: `jshook.Handle`, `jsadmission.Handle`); dispatch on `Result.Outcome` (nothing to recover: the next call gets a fresh VM); handle `ErrVMUnavailable` from `Runner.Invoke` by retrying later (events) or by your failure policy (admission), and `ErrUnknownKey` as "dropped". Keep no state in the script between calls: every call starts from the snapshot.
4. Drop the script with `Scripts.Drop` when the resource goes away.
5. Add a test with `jsregistry.NewRegistry()` (or a `jsrun.Runner` of your own) and carry the rule ID in a comment above it.

## Rules

- **R1** Run user JavaScript at runtime only through `Runner.Invoke` (the registry runs it in the function passed to `Registry.Call`), each call on its own VM restored from the snapshot of the key; calls of one key run in parallel.
  Why: one place for the call semaphore, panic recovery and outcome classification; a VM per call needs no per-key lock, so a slow call does not hold up the next one.
  Gate: `TestRegistry_CallsOfOneKeyRunInParallel`; that nobody runs JS around the port is missing → GATE-4. See also js-execution.R2.
- **R2** Throw the VM of a call away whatever the outcome, and never recover or rebuild a script after a call. A panic, a trap, a timeout or the memory limit end only that call; the next call starts fresh from the same snapshot, and `State.Recoveries` does not change.
  Why: a VM after a trap is in an unknown state, and a fresh one costs a quarter of a millisecond, so no caller and no adapter has to know a cause-to-restart table.
  Gate: `TestRegistry_Invoke_EveryOutcomeGetsAFreshInstance`, `TestRegistry_CancelledCall_NextCallGetsFreshInstance`, `TestDispatcher_PanicInHandle_RetriesOnFreshVM`.
- **R3** Resolve and hash the source in the controller, never in the registry.
  Why: the registry stays independent of source loading; the hash drives rebuilds.
  Gate: `TestRegistry_RestartOnSourceChange`.
- **R4** Prepare a script again from the `jsrun.Spec` handed to `Ensure` when `Spec.ResetToken` changes, with reason manual.
  Why: the manual restart annotation is a token, not a call; the port has no `Restart`.
  Gate: `TestRegistry_Ensure_ResetTokenRebuildsFromCachedSpec`, `TestRegistry_Ensure_ResetTokenRebuildsAsManual`.
- **R5** Keep one prepared script per key across reconciles while the spec is unchanged, and start every call from its snapshot: the state right after the top-level code ran. Nothing a call changes reaches the next call.
  Why: preparing (module load, top-level code) costs milliseconds and is done once; calls must not see each other's state, so a failed or hostile call cannot poison the next (js-execution.R16).
  Gate: `TestRegistry_EveryCallStartsFromSnapshot`.
- **R6** Hold the registry lock only for map access, never while user JavaScript runs.
  Why: a slow build or call of one key must not block other keys.
  Gate: `TestRegistry_Concurrent_CallsBuildsAndDrop`, `TestRegistry_Ensure_HangingBuildDoesNotBlockOtherKeys`.
- **R7** Serialise builds per key with the build lock.
  Why: two reconciles of one key must not race on construction.
  Gate: `TestRegistry_Concurrent_CallsBuildsAndDrop`.
- **R8** Replace or drop a prepared script without waiting for running calls: each call owns its VM and closes it itself, and a snapshot is never written after it was taken.
  Why: a call must not block `Ensure` or `Drop`, and a running call must not see its script change under it.
  Gate: `TestRegistry_Concurrent_CallsBuildsAndDrop`, `TestSnapshot_ConcurrentRestores`.
- **R9** Touch `Prepared.Snapshot` only inside `internal/jsregistry`; callers see only `jsrun.State`.
  Why: the field is exported for tests, but restoring VMs is the registry's job. Callers cannot reach it, because they may not import the registry (js-execution.R10). Tests and `cmd` may.
  Gate: `TestImportBoundary_CallersUseOnlyRunnerPort`.
- **R10** Cache per-VM data in `State.Meta` through `PostBuildHook`, not by calling the VM from a reconcile.
  Why: reconciles must not run user JavaScript outside the build.
  Gate: review only — a convention about where state lives.
- **R11** Record every preparation after the first with its reason in `State.Recoveries` (the per-reason counters and the history ring).
  Why: operators need to see why a script was prepared again; the controllers map `Recoveries` onto the CRD status fields.
  Gate: `TestRegistry_RecoveryHistory_RingAndCounters`.
- **R12** Rebuild a VM when `Ensure` gets other effective limits, even with an unchanged source hash.
  Why: a changed `spec.limits` must take effect; zero fields count as the defaults, so an explicit default is no change. The restart reason is `limits-changed`.
  Gate: `TestRegistry_Ensure_RebuildsOnLimitsChange`.
- **R13** Bound every build by the timeout limit of its spec.
  Why: a hanging module or `PostBuild` must not hold the per-key build lock forever.
  Gate: `TestRegistry_Ensure_RebuildHasDeadlineAndFailureHoldsNoScript`, `TestRegistry_Ensure_HangingBuildDoesNotBlockOtherKeys`.
- **R14** Key every registry entry by `jsrun.Key`, which carries the kind next to the name.
  Why: both kinds are cluster-scoped and share one registry, so a JSHook and a JSAdmission of the same name must not replace each other's VM.
  Gate: `TestRegistry_SameNameInBothKindsCoexists`.
- **R15** Never wait for a build in a reconcile or a call: ask `Scripts.Ensure`, which reports Ready, Preparing or Failed at once.
  Why: a hanging top-level `while(true){}` must not stall the reconcile worker of other hooks.
  Gate: `TestRegistry_Ensure_BuildsAsyncAndNotifies`, `TestRegistry_Ensure_HangingBuildDoesNotBlockOtherKeys`, `TestReconcile_HangingBuildDoesNotBlockOtherHook`.
- **R16** Cancel the running build of a key through its context when `Ensure` gets another spec, and start the new build.
  Why: a fixed source must not wait for the stuck one to run into its deadline.
  Gate: `TestRegistry_Ensure_NewOptsCancelRunningBuild`, `TestRegistry_Concurrent_EnsureCallDrop`.
- **R17** Run one build/retry loop per key: a Failed key is retried only after its backoff, changed options rebuild at once.
  Why: a bad source must not be rebuilt in a tight loop, and a fix must not wait for the backoff.
  Gate: `TestRegistry_Ensure_BrokenBacksOffAndSourceChangeRebuildsAtOnce`, `TestBackoff_DoublesCapsAndJitters`, `TestReconcile_BrokenBuild_BacksOffAndSourceChangeRebuildsAtOnce`, `TestFlags_BuildBackoffReachesReconcilers`, `TestFlags_BuildBackoffRejectsNonsense`.
  Base and cap are the operator flags `--build-backoff-base` (1s) and `--build-backoff-max` (5m); both reconcilers hand them to `Ensure`.
- **R18** Notify the key on the `Scripts.Watch` channel of its kind when a build ends, whether it installed a script or failed.
  Why: reconciles do not wait, so the controller of the kind needs the event to publish the new state.
  Gate: `TestRegistry_Ensure_BuildsAsyncAndNotifies`, `TestRegistry_Watch_DeliversOnlyOwnKind`.
- **R19** Hold no script in a Failed entry, and let `Runner.Invoke` return `ErrVMUnavailable` at once for it and for a key whose first build still runs. A build for changed options or a changed reset token keeps the old script serving until the new one is installed; every failed build drops the old script.
  Why: a script whose rebuild failed must not keep serving the old code as if nothing happened; callers must see "not ready" and apply their own policy (requeue, `failurePolicy`).
  Gate: `TestRegistry_Call_WithoutScript_IsErrVMUnavailable`, `TestRegistry_Ensure_RebuildHasDeadlineAndFailureHoldsNoScript`, `TestDispatcher_NoVM_KeepsEventsAndDeliversAfterBuild`.
- **R20** Bound the calls that run at the same time, over all keys, by one process-wide semaphore: the operator flag `--max-concurrent-calls` (default `jsregistry.DefaultMaxConcurrentCalls`, 16; zero or less is rejected). A call waits for a slot inside its own context; a context that ends while it waits gives `OutcomeCancelled` and runs nothing.
  Why: every call has its own VM with up to `spec.limits.memoryMB` of heap, so without a bound a burst of admission requests or events multiplies the memory of the operator; the bound makes it `max-concurrent-calls` × memory limit.
  Gate: `TestRegistry_Semaphore_BoundsConcurrentCalls`, `TestFlags_MaxConcurrentCalls_DefaultsAndRejectsNonsense`.

## Decisions

- **A build runs user JavaScript under a per-key lock and never under the registry lock.** Status: accepted (carried over from the block, no date or name recorded).
  Why: a slow build of one key must not block other keys. Not taken: not recorded anywhere.
- **`Runner.Invoke` (`Registry.Call` in the adapter) is the one place for the call semaphore, panic recovery and outcome classification.** Status: accepted (carried over from the block, no date or name recorded).
  Why: callers must not each carry their own copy. Not taken: not recorded anywhere.
- **The registry caches the whole `jsrun.Spec` and never loads sources.** Status: accepted (carried over from the block, no date or name recorded).
  Why: the registry stays independent of source loading. Not taken: not recorded anywhere.

- **Builds run asynchronously; a newer build cancels a running one.** Status: proposed.
  Why: a build runs user JavaScript that may hang; a reconcile or call that waits for it stalls other hooks, and a stuck build blocks the fix. Every key is in one phase, Ready, Preparing or Failed, and `Scripts.Ensure` reports it without waiting.
  A changed reset token builds in the background too, while the old script serves; a failed build leaves the key Failed without a script. Callers meet `ErrVMUnavailable`: the dispatcher requeues the event, the admission server applies `failurePolicy`.
  Not taken: a synchronous build with a reconcile deadline, because the build lock stays held by the hanging build and the reconcile worker still waits.

- **Callers depend on the port `jsrun.Runner`, and the registry is its first adapter.** Status: accepted (2026-10, project, EXEC-6).
  Why: callers that name `jsregistry.Registry` or `jsengine.VM` tie the operator to one engine; the port (ensure, invoke by key and export, drop, watch) hides VMs, snapshots and the engine.
  Not taken: a `Call(fn(vm))` callback in the port, because it hands the engine to the caller. A fake of the port for caller tests, because the real registry is cheap and a fake would repeat its states.

- **Split the port into `jsrun.Runner` (data path) and `jsrun.Scripts` (lifecycle), and keep VM words out of it.** Status: proposed (2026-10-01, open).
  Why: the dispatcher and the admission server need only `Invoke`; controllers need only `Ensure`, `Drop` and `Watch`; one wide interface makes every caller depend on the other half. The manual annotation is `Spec.ResetToken`, a change of it prepares again like a changed source; a controller learns about a script only through the neutral `State` that `Ensure` returns; an unknown key is `ErrUnknownKey` of `Invoke`. The CRD keeps `status.instance.restartsByReason` and `recentRestarts`; the controllers map `Recoveries` onto them (API-10).
  Not taken: a `State(key)` method on `Scripts`, because no caller needs it. One wide interface with documented "controllers only" methods, because the compiler cannot hold the boundary then.

- **Run every call single shot: a fresh VM from the snapshot of the key, behind a process-wide semaphore.** Status: proposed (2026-10-01, open) (EXEC-9, the way jspolicy runs its policies).
  Why: a VM per call needs no per-key lock, so calls of one hook or policy run in parallel and a slow call does not hold up the admission webhook; nothing has to be recovered after a call, and a call cannot leave state that breaks the next. A restore from the sparse snapshot costs about 0.24 ms (`BenchmarkSnapshot_Shot`) against 12 µs for a warm call on a kept VM; that is small against an admission round trip. The semaphore (R20) bounds the memory that parallel calls take.
  Not taken: one long-lived VM per key, because its lock serialises calls and a trap needs a rebuild. A pool of warm VMs per key, because a pooled VM carries state from call to call.

## Open

Tracked in [backlog](../backlog.md): REG-3 to REG-5; gate GATE-4.
