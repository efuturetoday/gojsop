---
id: status-conditions
status: accepted
entrypoints:
  - conditions.ClassifyBuildError
  - jslifecycle.RestartHistoryFor
  - jsregistry.Registry.GetOrLoad
---

# Status and Conditions

This block describes how gojsop reports the state of a JSHook or JSAdmission in
its CRD status.

Status holds only what the controller observed at reconcile time, plus a
projection of the restart log of the registry. There is one condition type,
`Ready`. It has two reasons, `Reconciled` and `Failed`. Richer detail lives in
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
field `status.instance.lastRestartReason` no longer exists. Do not refer to it.

Event reasons come from a finite set in `internal/conditions`. The Event
recorder dedupes on reason and message, so per-request detail stays out of
Event messages. It goes to conditions, status fields and logs.

Each reconciler is the only writer of the status of its own kind. The
dispatcher, the admission server, the registry and the lifecycle code never
write status.

## Rules

- **R1** Set `Ready` with `apimeta.SetStatusCondition`, a Reason constant from
  `internal/conditions`, and always `ObservedGeneration`, on success and on
  failure.
  Why: users and tools compare `observedGeneration` with the generation to see
  whether status is current.
  Gate: missing → GATE-8, GATE-9.
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
- **R4** Take restart counters and history from the registry through
  `jslifecycle.RestartHistoryFor`. Never keep a second count in a controller.
  Why: two counts drift apart.
  Gate: `TestRegistry_RestartHistory_RingAndCounters`, `TestRescue_*`.
- **R5** Put only the reconcile outcome into `lastReconcile`, never a runtime
  call result.
  Why: the removed fields lied about what the controller knew.
  Gate: missing → GATE-9. Violated today → STAT-1.
- **R6** Declare no CRD field that is neither implemented nor surfaced in
  status. This is a candidate project rule, not yet enforced.
  Why: not recorded.
  Gate: missing → GATE-10.

## Rejected

Rejected alternatives are not recorded beyond the two commit messages named
above.

## Open

Tracked in [backlog](../backlog.md): STAT-1 to STAT-5, API-1, API-5; gates
GATE-8 to GATE-11, GATE-13.
