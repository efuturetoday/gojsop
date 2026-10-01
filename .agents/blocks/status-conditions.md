# Block: Status and Conditions

Status: accepted

## Decision

CRD status reports only what the controller observed at reconcile time, plus a
projection of the registry's restart log. One condition type, `Ready`, carries
the result; richer detail lives in typed status fields and in corev1.Events.

- Status is split by meaning. `status.lastReconcile` says whether the controller
  could load and register the hook/policy; it says nothing about whether
  `handle()`/`validate()` ran. Commit c7d58ac replaced `LastExecution` /
  `LastReview` because the failure paths wrote to fields documented as "last
  handle()/validate() call" that no dispatcher ever wrote ("the schema lied").
  Runtime telemetry is deferred to a later phase (commit message, STAT-4).
- Restart bookkeeping is per-reason counters plus a capped history, not one
  counter and one string (commit 025a482: six triggers had collapsed into
  `RestartCount` + `LastReason`). The CRD mirrors the registry, newest first so
  print columns can read `recentRestarts[0]`.
- Event reasons are a finite set; per-request detail stays out of Event
  messages and goes to conditions, status and logs (`internal/conditions/conditions.go:29-33`).
- Rejected alternatives: not recorded beyond the two commit messages above.
  Why only `Ready` and two Ready reasons: reason not recorded.

## Code

- Vocabulary: `internal/conditions/conditions.go`. `Ready` (:14),
  `ReasonReconciled` (:17), `ReasonFailed` (:21), `ManualRestartAnnotation`
  `gojsop.io/restart` (:26), Event reasons (:34-55), `ClassifyBuildError` (:64)
  mapping typed jsregistry sentinels to an Event reason and a fixed message.
  There are no helper functions that write conditions; the package is constants
  plus the classifier.
- Types: `api/v1alpha1/js_shared.go` (`JSRestartEvent` :113, `JSInstanceStatus`
  :133, `JSReconcileStatus` :168), `jshook_types.go:35` (`JSHookStatus`),
  `jsadmission_types.go:119` (`JSAdmissionStatus`).
- Fields on both kinds: `observedGeneration`, `conditions []metav1.Condition`,
  `instance` (`startedAt`, `sourceHash`, `restartsByReason map[string]int32`,
  `recentRestarts` capped at 20, `manualRestartToken`), `lastReconcile`
  (`time`, `error`).
- JSHook only: `bindings []string` (`jshook_types.go:46`), built by
  `summarizeBindings` (`internal/jshook/controller/controller.go:311`) from the
  decoded `config()`. Examples asserted in
  `test/integration/jshook_controller_test.go:104-105`
  (`kubernetes:v1/ConfigMap/watch-cm`, `onStartup:5`).
- JSAdmission only: `webhookPath`, `webhookConfigName`
  (`jsadmission_types.go` status struct, set at `jsadmission/controller/controller.go:250-255`).
- Restart ring: `historyCap = 20` (`internal/jsregistry/registry.go:63`);
  `installNew` bumps `RestartsByReason[reason]` and appends to `History`
  oldest first, trimming to the cap (`registry.go:276-297`). Reasons are the
  `RestartReason` constants `source-changed`, `memory-limit`, `panic`,
  `timeout`, `timeout-streak`, `manual` (`registry.go:31-36`).
  `jslifecycle.RestartHistoryFor` (`internal/jslifecycle/jslifecycle.go:71`)
  converts to the CRD shape and reverses to newest first.
- Print columns: Ready, Reason (priority 1), LastRestart, RestartedAt
  (priority 1) at `jshook_types.go:68-71`, `jsadmission_types.go:158-161`.
- observedGeneration: written both on the condition
  (`ObservedGeneration: hook.Generation`, `jshook/controller/controller.go:247`)
  and on `status.observedGeneration` (:249), on success and on failure
  (:285-287). The controller also reads it to decide whether to re-subscribe
  (:149, :224).

Single allowed writer: the owning reconciler, via `r.Status().Update`.
- JSHook: success `jshook/controller/controller.go:259`, failure through
  `fail()` (:277-292).
- JSAdmission: success `jsadmission/controller/controller.go:264`, failure
  through `failAdmission()` (:284-296).
These are the only `Status()` call sites in `internal` and `cmd` (grep).
Dispatcher, admission server, registry and lifecycle code do not write status.

## Rules

- Do: set `Ready` through `apimeta.SetStatusCondition` with `Type: conditions.Ready`
  and a Reason from `internal/conditions`, and always set `ObservedGeneration`.
- Do: write status only from the kind's own reconciler and only with
  `r.Status().Update`.
- Do: keep Event messages from a finite set; use `conditions.ClassifyBuildError`
  for build errors.
- Do: source restart counters and history from the registry
  (`jslifecycle.RestartHistoryFor`), never keep a second count in the controller.
- Don't: put runtime call results into `lastReconcile`; it is reconcile outcome only.
- Don't: declare a CRD field that is neither implemented nor surfaced in status
  (candidate project rule, not yet enforced; see Open).
- Don't: reference `status.instance.lastRestartReason`; the field no longer
  exists (stale comments at `api/v1alpha1/js_shared.go:102`,
  `internal/jsregistry/registry.go:20`).

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| Build error to Event reason mapping | `go test ./internal/conditions -run TestClassifyBuildError` | yes (`make test` in `.github/workflows/test.yml`) |
| Restart counters and 20-entry ring | `go test ./internal/jsregistry -run TestRegistry_RestartHistory_RingAndCounters` | yes |
| Rescue emits Restarted / RescueFailed events | `go test ./internal/jslifecycle -run TestRescue` | yes |
| JSHook status: Ready, bindings, instance.sourceHash | `make test`, Ginkgo "JSHook Controller" `test/integration/jshook_controller_test.go:85` | yes |
| JSAdmission reconciles | `test/integration/jsadmission_controller_test.go:78` (asserts only success; no status field checks found) | partial |
| Lint (`revive`, `goconst`, `lll`, etc., `.golangci.yml`) | `make lint` (`lint.yml`) | yes, but no rule on conditions |
| Condition Reasons only from `internal/conditions` constants | proposed: grep or `go/analysis` test failing on `Reason:` string literals outside `internal/conditions` | missing |
| Only reconcilers call `Status()` | proposed: test that greps `internal/` for `.Status()` outside `*/controller/` | missing |
| Every ready reconcile has `observedGeneration == generation` | proposed: envtest assertion in both integration suites | missing |
| `RestartReason` values match CRD doc enum (`js_shared.go:118`) | proposed: unit test comparing constants to the documented list or a kubebuilder `Enum` marker | missing |
| `lastReconcile.error` cleared after a successful reconcile | proposed: envtest fail-then-fix case | missing |
| Every spec field implemented or surfaced in status | proposed: table test listing spec fields with an owner | missing |

Verified today: the Reason-only-from-constants rule holds. `Reason:` appears in
non-test code only with `conditions.ReasonReconciled` / `conditions.ReasonFailed`
(jshook controller :245, :283; jsadmission :245, :290). No `Status()` call
sites outside the two controllers. `make test` also runs `manifests generate fmt vet`
(`Makefile:61`). E2E (`make test-e2e`, `test-e2e.yml`) has no status checks found.

Gates marked `missing` are commitments, not facts.

## Open

Tracked in [backlog](../backlog.md): STAT-1 to STAT-5, API-1, API-5; gates GATE-8 to GATE-11, GATE-13.
