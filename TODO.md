# TODO

Curated whiteboard review of the gojsop operator. Grouped by category;
each item cites the relevant file:line. No fixes prescribed — pick what
to do next.

## Naming drift (CRD ↔ internal)

`spec.resources` / `JSResources` was renamed to `spec.limits` /
`JSLimits` to match the runtime's `jsengine.Limits` vocabulary while
still on `v1alpha1` — no other drift between the renamed
`jsengine`/`jsregistry` internals and the CRD surface remains.

`AdmissionRule.Resources` (`api/v1alpha1/jsadmission_types.go:40`) is
the unrelated k8s "rule resources" list (e.g. `pods`) — kept as-is.

## 1. Smells

- **Restart bookkeeping is shallow.** `ManagedVM` keeps only
  `RestartCount` + `LastReason`; no per-trigger counters (OOM vs
  panic vs timeout streak vs manual), no history ring.
- **`KubeHost` is a process-wide singleton.** One `Ctx`/`Dyn`/`Mapper`
  shared across all hooks; per-hook ServiceAccount scoping (Phase 2)
  will require touching every call site.
  `internal/jsengine/kubehost/kubehost.go:24-31`.
- **Failure path overwrites `LastExecution`.** `controller.fail()`
  writes both Conditions and `LastExecution.Error`, mixing reconcile
  failures with hook-execution telemetry. Same shape on the admission
  side via `failAdmission`.
- **5s hard-coded backoff on every fail path.** `RequeueAfter:
  5*time.Second` regardless of error class — bad source URL and a
  transient API error get the same retry shape.
- **Admission rescue is partial.** Panic and OOM in `validate()` /
  `mutate()` now rebuild the VM via `jslifecycle.Rescue`, symmetric
  to the dispatcher. Single timeouts still don't rescue: the review
  goroutine is still alive and still holds `mi.CallMu`, so closing
  the runtime would race with an active eval. Until JS is
  interruptible (separate engine-level work) a stuck policy waits
  for its own call to finish.
  `internal/jsadmission/server.go:review`.
## 2. Missing

- **No source loaders beyond Inline.** CRD shapes `configMapRef` and
  `oci` exist but only `InlineLoader` is registered; users referencing
  a ConfigMap silently get nothing.
  `internal/jssource/`, `cmd/main.go`.
- **Schedule bindings never fire.** `Config.Schedule`,
  `ScheduleBinding`, `OnStartup` are decoded and surfaced in
  `status.bindings` but the dispatcher only watches Kubernetes
  resources — no cron, no startup tick.
  `internal/jshook/dispatcher/dispatcher.go`.
- **`jqFilter` declared, not enforced.** `KubernetesBinding.JQFilter`
  is typed but the dispatcher enqueues every event regardless.
- **No Prometheus metrics.** `cmd/main.go` exposes
  `--metrics-bind-address` but neither dispatcher nor admission
  server registers anything (handle count/duration/restart counters,
  review latency, allow/deny counts). flant/shell-operator publishes
  all of these. With events landed, this is the natural counterpart:
  events answer "did something interesting happen", metrics answer
  "how often, how slow".
- **JS execution is not interruptible.** `Limits.TimeoutSeconds` is a
  Go-side context deadline; the QuickJS instance keeps running an
  infinite loop and the worker goroutine is held until it returns.
  Three timeouts trigger a hard restart but the in-flight goroutine
  leaks until QuickJS returns. Same shape on the admission server's
  review goroutine — `ReviewTimeout` fires, but the qjs call leaks
  until it finishes. Build-time hangs (`while(true)` at module top
  level or inside `config()`) hold the per-key build mutex with no
  event ever fired, since the build never returns. Engine-level
  fix (qjs interrupt callback or runtime-close from a watchdog).
- **No leader-election awareness in the dispatcher.** Informers spin
  up locally on the leader; on failover the new leader rebuilds them
  with a fresh Synchronization. Events arriving in the gap are lost.
- **No finalizer on JSHook / JSAdmission.** Deletion is detected via
  `IsNotFound` in the next reconcile; an in-flight handle can outlive
  the CR by up to one reconcile loop. The RBAC declares
  `jshooks/finalizers` but nothing uses it.
  `internal/jshook/controller/controller.go:74,105-115`.
- **No per-hook RBAC narrowing.** Wildcard
  `groups=*,resources=*` in `controller.go:77`; a misbehaving JS
  policy can touch anything.
- **JSAdmission `lastReview` only populated on failure.**
  `failAdmission` writes `status.lastReview.error`; the success path
  doesn't write a `time` or duration, so users still can't tell if a
  policy is being called or how long the happy path takes.
  `api/v1alpha1/jsadmission_types.go`,
  `internal/jsadmission/server.go:review`.
- **`samples/core_v1alpha1_jshook.yaml` likely doesn't fully run.**
  Schedule/jqFilter/queue/allowFailure fields wouldn't have any
  effect; samples should match what's implemented.

## 3. Unclear

- **"Synchronization" is jargon without explanation.** Appears in
  `bindingctx.go`, the CRD, and behaviour, but is documented nowhere
  a user looks (no README, no CRD description, no sample comment).
  Users from outside flant/shell-operator must read source.
- **`executeHookOnEvent` vs `executeHookOnSynchronization` interaction.**
  Empty `executeHookOnEvent` defaults to "all events" but its
  relationship to snapshot delivery is unspecified.
- **`namespace.nameSelector` vs binding-level `nameSelector`.** Both
  selector shapes coexist on `KubernetesBinding`; precedence and
  combination rules aren't written down.
- **`AllowFailure`, `Queue` accepted on bindings, never read.**
  Users will assume they work.
- **`BindingContext` shape is type-dependent.** `Object` populated
  for events, `Objects` for Synchronization, `Snapshots` never. JS
  has to inspect `type` to know which to read.
- **CRD field docs are thin.** `kubectl explain jshook.spec.source`
  doesn't say `configMapRef` and `oci` aren't implemented.
- **`ManualRestartAnnotation` value semantics.** Any new value
  triggers a restart; convention (timestamp / hash / UUID) is left
  to the user with no guidance in the CRD or sample.
- **`spec.source.inline` is etcd-visible.** Fine for code, dangerous
  for secrets, with no warning in the CRD description.

## 4. Too complex

- **Authoring surface mixes "what works" with "what's typed".** A
  user has to read source to learn schedule/jqFilter/queue/
  allowFailure are placeholders. The CRD shape promises a richer
  feature set than the runtime delivers.
- **Two concurrency models in one operator.** JSHook = informer
  queue + persistent VM + FIFO worker; JSAdmission = synchronous
  webhook on the same registry. Explaining "the VM is shared, but
  JSHook serializes via worker and admission via per-VM lock" takes
  a paragraph.
- **Restart contract has four triggers and one knob.** OOM / panic /
  timeout-streak (3) / manual; only the `restart` annotation is
  user-facing. No `spec.restartPolicy` to opt out, tighten the
  streak threshold, or freeze the VM after N restarts.
- **JS error surface is a string.** `Status.Condition.Message` is
  the main feedback path. Build-time gets a small assist now —
  `EntrypointMissing` events name the missing export, and typed
  build errors classify load-module / post-build / bind-host /
  new-vm cleanly — but `handle()`/`validate()` runtime errors still
  flatten to one truncated line with no stack/line-number/export.
- **Inline source ergonomics.** Multi-line JS in YAML is painful,
  and there's no `kubectl gojsop validate hook.yaml` or syntax check
  on admission of the CR itself.

## Suggested first three

1. **Schedule bindings actually fire** — the CRD already promises it
   and the type is wired through to `status.bindings`.
2. **Prometheus metrics** — at minimum: handle count, handle
   duration, restart count by reason, admission review latency.
3. **Non-Inline source loaders (ConfigMap first, OCI later)** —
   moves the operator from "demo" to "usable in anger" and unlocks
   GitOps-friendly hook delivery.
