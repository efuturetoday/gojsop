---
id: status-conditions
status: accepted
entrypoints:
  - conditions.ClassifyBuildError
  - jslifecycle.RestartHistoryFor
  - jsregistry.Registry.Ensure
---

# Status and Conditions

This aspect describes how gojsop reports the state of a JSHook or JSAdmission in
its CRD status.

Status holds only what the controller observed at reconcile time, plus a
projection of the recovery log (`jsrun.State.Recoveries`) of the runner. There is one condition type,
`Ready`. It has five reasons: `Reconciled`, `Failed`, `WebhookSyncFailed` (the registrar could not write the central webhook configurations), `Building` (the JS instance is being built, nothing failed) and `BuildFailed` (the last build failed, the controller retries with backoff). Richer detail lives in
typed status fields and in Kubernetes Events. Why only one condition type and
two reasons is not recorded.

Status is split by meaning. `status.lastReconcile` says whether the controller
could load and register the script. It says nothing about whether `handle()` or
`validate()` ran. Commit c7d58ac removed `LastExecution` and `LastReview`: the
failure paths wrote to fields documented as "last call" that no dispatcher ever
wrote, so the schema lied. Runtime telemetry is deferred to a later phase.

Restart bookkeeping is a counter per reason plus a capped history of 20
entries. Commit 025a482 replaced a single counter and a single string, because
six triggers had collapsed into them. The registry owns the log. The CRD
mirrors it, newest first, so print columns can read `recentRestarts[0]`. The
CRD fields live under `status.script` (api-design Decisions).

Event reasons come from a finite set in `internal/conditions`. The Event
recorder dedupes on reason and message, so per-request detail stays out of
Event messages. It goes to conditions, status fields and logs.

Each reconciler is the only writer of the status of its own kind. The
dispatcher, the admission server, the registry and the lifecycle code never
write status.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once, so nobody builds it a second time? | `conditions.ClassifyBuildError` and the Reason constants of `internal/conditions` (`conditions.ReasonReconciled`, `conditions.ReasonFailed`, `conditions.ReasonWebhookSyncFailed`, `conditions.ReasonBuilding`, `conditions.ReasonBuildFailed`), `jslifecycle.RestartHistoryFor` for restart data, `apimeta.SetStatusCondition` from apimachinery for the condition. Searched `internal/` for `SetStatusCondition`, `Status().Update`, `Reason:`; both reconcilers use only these. |
| example | Which real use in the code should others copy? | `jshook/controller.JSHookReconciler.Reconcile` and its `fail` method. |
| test helper | How does a test use the aspect without effort? | `jsregistry.NewRegistry` for the registry log in unit tests; the envtest suite in `test/integration` for reconcilers. No helper asserts a status condition. Searched `*_test.go` for `Conditions`, `ObservedGeneration`: no hit. |
| sides | Which sides does it touch? | Back end only: the two reconcilers, the registry, and the CRD types in `api/v1alpha1`. Users read the result through kubectl. |
| tie | How do the sides stay in step? | Generated: `make manifests` builds the CRD YAML from the Go types with controller-gen. The enum of the restart reason is checked against the `jsrun` constants by `TestRecoveryReasons_MatchCRDEnum`. |

## How to use it

Add a new failure cause to status:

1. Add a sentinel error in `internal/jsregistry` and map it in `conditions.ClassifyBuildError` to an Event reason and message from the finite set.
2. Call the `fail` method of the reconciler of the kind. It sets `Ready=False` with `conditions.ReasonFailed` and `ObservedGeneration`.
3. Add a case to `TestClassifyBuildError`.
4. Run `make test lint`.

## Rules

- **R1** Set `Ready` with `apimeta.SetStatusCondition`, a Reason constant from
  `internal/conditions`, and always `ObservedGeneration`, on success and on
  failure.
  Why: users and tools compare `observedGeneration` with the generation to see
  whether status is current.
  Gate: `integration.TestControllers` (the JSHook fail-then-fix case); the Reason constants: missing → GATE-8.
- **R2** Write status only from the reconciler of that kind, and only with
  `Status().Update`.
  Why: one writer per status avoids conflicting updates. A further reason is
  not recorded.
  Gate: missing → GATE-8.
- **R3** Keep Event messages from a finite set. Map build errors with
  `conditions.ClassifyBuildError`.
  Why: the recorder dedupes on reason and message, so unbounded messages flood
  the event stream.
  Gate: `TestClassifyBuildError`.
- **R4** Take restart counters and history from the runner's `State.Recoveries` through
  `jslifecycle.RestartHistoryFor`, which maps them onto the CRD fields `restartsByReason` and `recentRestarts`. Never keep a second count in a controller.
  Why: two counts drift apart. The neutral `Recoveries` of the port and the user's word "restart" meet in this one function.
  Gate: `TestRegistry_RecoveryHistory_RingAndCounters`.
- **R5** Put only the reconcile outcome into `lastReconcile`, never a runtime
  call result. Write it when the outcome changes: every failure with its
  error, and the first success after a failure with an empty error, through
  `jslifecycle.ReconcileSucceeded`. A steady success writes nothing.
  Why: the removed fields lied about what the controller knew; a time written
  on every success would change status on every reconcile and trigger the next.
  Gate: `TestReconcileSucceeded_ClearsErrorOnceThenKeepsStatus`, `integration.TestControllers` (the JSHook fail-then-fix case).
- **R7** Report a script that is not ready as `Ready=False` with reason `Building` while the build runs and `BuildFailed`, with the build error as message, after it failed. Never wait for the build, and requeue a failed build only after the registry's backoff.
  Why: users must tell a slow or hanging build from a rejected spec, and a failing build must not be retried in a tight loop.
  Gate: `TestReconcile_HangingBuildDoesNotBlockOtherHook`, `TestReconcile_BrokenBuild_BacksOffAndSourceChangeRebuildsAtOnce`, `TestReconcile_BuildStates_ShowBuildingThenBuildFailed`.
- **R6** (withdrawn) Declare no CRD field that is neither implemented nor
  surfaced in status.
  Why: it repeated api-design.R6, which now holds it.
  Gate: missing → GATE-10.

## Decisions

- **One condition type `Ready` with the reasons `Reconciled`, `Failed`, `WebhookSyncFailed`, `Building` and `BuildFailed`.** Status: accepted (2026-10-01, migrated from the old block; original date not recorded).
  Why: richer detail lives in typed status fields and Events. The original reason is not recorded.
  Not taken: further condition types, because no source records a reason.
- **Remove `LastExecution` and `LastReview` from status (commit c7d58ac).** Status: accepted (2026-10-01, migrated from the old block).
  Why: the failure paths wrote fields documented as "last call" that no dispatcher wrote, so the schema lied.
  Not taken: keep the fields until runtime telemetry exists, because they misreport state.
- **Keep a counter per reason and a history capped at 20 entries (commit 025a482).** Status: accepted (2026-10-01, migrated from the old block).
  Why: six triggers had collapsed into one counter and one string.
  Not taken: a single counter and a single string, because they lose the cause.

- **Map the neutral `jsrun.State.Recoveries` onto the CRD fields `status.script.restartsByReason` and `recentRestarts` in the controllers.** Status: accepted (2026-10, API-10).
  Why: the port speaks of recoveries, the user of restarts; one function (`jslifecycle.RestartHistoryFor`) translates.

## Open

Tracked in [backlog](../backlog.md): STAT-2, STAT-4, API-1; gates
GATE-8, GATE-10, GATE-13.
