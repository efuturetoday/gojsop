# Backlog

All open items in one place, grouped by aspect. Sources: the former `TODO.md`, the
`Open` and `Gates` sections of the blocks, and the maturity review of
2026-10-01.

Keys are stable: `<ASPECT>-<n>`. Never reuse or renumber a key. When an item
is done, delete it here and mention the key in the commit message. Blocks
reference these keys instead of keeping their own lists.

Types: `bug` (wrong behaviour), `gap` (missing feature), `gate` (missing
enforcement), `debt` (code or rule violation), `doc` (missing or wrong docs),
`decision` (needs a choice first).

| Aspect | Prefix | Block |
|--------|--------|-------|
| JS execution | EXEC | [js-execution](blocks/js-execution.md) |
| JS registry and restarts | REG | [js-registry](blocks/js-registry.md) |
| Status and conditions | STAT | [status-conditions](blocks/status-conditions.md) |
| JS sources | SRC | planned: js-sources |
| Hook dispatch and bindings | DISP | planned: hook-dispatch |
| CRD API surface | API | planned: api-design |
| Operations (metrics, RBAC, reconcile) | OPS | planned |
| Gates and test infrastructure | GATE | planned: testing |
| Documentation | DOC | none |

## EXEC: JS execution

- **EXEC-1** `debt` qjs imported outside `jsengine`.
  `internal/jsadmission/handle.go:7` imports `fastschema/qjs`. Add a
  `jsengine` helper that decodes the result into a Go struct, then drop the
  import. Blocks GATE-2.
- **EXEC-2** `bug` `kube.*` calls not bound by `timeoutSeconds`. `KubeHost`
  uses its own parent `h.Ctx` (`internal/jsengine/kubehost/kubehost.go:91-96`),
  not the per-call deadline. Not verified at runtime.
- **EXEC-3** `gate` `CloseOnContextDone` overhead unmeasured. The planned
  benchmark never ran. Fallback if too slow: fork qjs for QuickJS interrupts.
- **EXEC-4** `gap` JS runtime errors are one truncated string. `handle()` and
  `validate()` errors have no stack, line number or export name.

## REG: JS registry and restarts

- **REG-1** `bug` Build-time hangs leak. A top-level `while(true)` or one in
  `config()` holds the per-key build lock, no event fires. `RestartByKey` needs
  one successful build first.
- **REG-2** `bug` Rescue build has no deadline. `RestartByKey` builds with
  `context.Background()` (`internal/jsregistry/registry.go:362`).
- **REG-3** `gap` No finalizer on JSHook / JSAdmission. Deletion is seen via
  NotFound on the next reconcile, an in-flight call can outlive the CR, and a
  mid-build `GetOrLoad` can install a zombie VM (`registry.go:317-319`). RBAC
  already declares `jshooks/finalizers`.
- **REG-4** `decision` Restart contract: six triggers (OOM, panic, timeout,
  timeout-streak, manual, source-changed), one user knob (`restart`
  annotation). No `spec.restartPolicy`. `ReasonTimeoutStreak` is marked
  "legacy" (`registry.go:28`); decide whether it stays.
- **REG-5** `decision` Uncommitted debug logging in `Registry.Drop` and
  "cleanup started/done" logs in both controllers. Keep, lower to V(1), or
  remove.
- **REG-6** `doc` Two concurrency models on one lock: JSHook uses informer
  queue plus FIFO worker, JSAdmission is a synchronous webhook. Document the
  asymmetry in the js-registry block.

## STAT: Status and conditions

- **STAT-1** `bug` `status.lastReconcile` not written on success. An old
  `lastReconcile.error` stays after recovery, `time` only set on failure
  (`internal/jshook/controller/controller.go:242-259`,
  `internal/jsadmission/controller/controller.go:242-264`). Not verified at
  runtime. Gate: GATE-9.
- **STAT-2** `decision` Only one condition type `Ready`, `Ready=False` has the
  single reason `Failed`. Users must read `message` or Events to tell causes
  apart.
- **STAT-3** `debt` Stale comments reference the removed
  `status.instance.lastRestartReason` (`api/v1alpha1/js_shared.go:102`,
  `internal/jsregistry/registry.go:20`).
- **STAT-4** `gap` No runtime telemetry in status. `lastExecution` and
  `lastReview` were removed in c7d58ac; users cannot see whether `handle()` or
  `validate()` runs or how long it takes. Replaces the stale TODO.md item
  "JSAdmission lastReview only populated on failure". See also OPS-1.
- **STAT-5** `bug` `status.bindings` lists schedule and onStartup bindings as
  if active. Fix with DISP-1 or mark them inactive.

## SRC: JS sources

- **SRC-1** `gap` No OCI loader. `spec.source.oci` is typed only and fails with
  "no loader matched" (SourceLoadFailed). `internal/jssource/`, `cmd/main.go`.
- **SRC-2** `doc` `spec.source.inline` is visible in etcd. Fine for code,
  dangerous for secrets. No warning in the CRD description.
- **SRC-3** `gap` Inline source ergonomics. Multi-line JS in YAML is painful.
  No `kubectl gojsop validate` and no syntax check when the CR is admitted.

## DISP: Hook dispatch and bindings

- **DISP-1** `gap` Schedule and onStartup bindings never fire. Decoded and
  shown in `status.bindings`, but the dispatcher only watches Kubernetes
  resources (`internal/jshook/dispatcher/dispatcher.go`).
- **DISP-2** `gap` `jqFilter` declared, not enforced. The dispatcher enqueues
  every event (`KubernetesBinding.JQFilter`).
- **DISP-3** `gap` `allowFailure` and `queue` accepted on bindings, never read.
- **DISP-4** `bug` No leader-election awareness. On failover the new leader
  rebuilds informers with a fresh Synchronization; events in the gap are lost.
- **DISP-5** `decision` `BindingContext` shape depends on type: `Object` for
  events, `Objects` for Synchronization, `Snapshots` never set.
- **DISP-6** `doc` Interaction of `executeHookOnEvent` and
  `executeHookOnSynchronization` unspecified. Empty `executeHookOnEvent` means
  "all events"; relation to snapshot delivery unclear.
- **DISP-7** `doc` Precedence of `namespace.nameSelector` versus binding-level
  `nameSelector` not written down.

## API: CRD API surface

- **API-1** `decision` Project rule: no CRD field without implementation or a
  status that shows it is inactive. Today violated by DISP-1, DISP-2, DISP-3,
  SRC-1. Gate: GATE-10.
- **API-2** `doc` CRD field docs are thin. `kubectl explain jshook.spec.source`
  does not say which sources work.
- **API-3** `doc` `gojsop.io/restart` annotation value semantics undocumented.
  Any new value restarts; no convention (timestamp, hash, UUID).
- **API-4** `debt` `samples/core_v1alpha1_jshook.yaml` uses fields without
  effect (schedule, jqFilter, queue, allowFailure). Samples should match what
  works.
- **API-5** `debt` `RestartReason` constants and the CRD doc list
  (`api/v1alpha1/js_shared.go:118`) can drift. Use a kubebuilder `Enum`
  marker. Gate: GATE-11.

## OPS: Operations

- **OPS-1** `gap` No Prometheus metrics. `--metrics-bind-address` exists, but
  nothing registers handle count and duration, restarts by reason, admission
  latency, allow and deny counts.
- **OPS-2** `gap` No per-hook RBAC narrowing. Wildcard `groups=*,resources=*`
  (`internal/jshook/controller/controller.go:77`); a bad script can touch
  anything.
- **OPS-3** `debt` Fixed 5 s `RequeueAfter` on every failure path. A bad
  source URL and a transient API error get the same retry.
- **OPS-4** `gap` No release process. No tags, no versioned image.

## GATE: Gates and test infrastructure

- **GATE-1** Add `make gates`: one target that runs all gates, identical in CI.
- **GATE-2** Import boundary via golangci `depguard` or an arch test: no
  `fastschema/qjs` or `wazero` outside `internal/jsengine/**`; `jsengine` used
  only by `jsregistry`, `jshook`, `jsadmission`. Blocked by EXEC-1.
- **GATE-3** Add `-race` to `make test`, plus a test with N goroutines on
  `Registry.Call` and concurrent `RestartByKey` / `Drop`.
- **GATE-4** `TestRegistry_Call_*`: outcomes OK, panic, cancelled, OOM, error,
  unknown key. No test references `Registry.Call` today.
- **GATE-5** Test in `internal/jsengine`: `for(;;){}` with a 100 ms deadline
  returns `ErrCancelled`, VM still closable.
- **GATE-6** Isolation test: a global set in VM A is not visible in VM B.
- **GATE-7** Check that admission VMs are built with `Factory.ForAdmission`
  (no `kube.apply` or `kube.delete`).
- **GATE-8** Status rules: `Reason:` only from `internal/conditions`
  constants, `Status()` only in `*/controller/`. Both hold today, nothing
  enforces them.
- **GATE-9** envtest: `observedGeneration == generation` after a ready
  reconcile; `lastReconcile.error` cleared after fail-then-fix (STAT-1).
- **GATE-10** Table test that lists every spec field with an owner (code path
  or status field) (API-1).
- **GATE-11** Test that `RestartReason` constants match the CRD enum (API-5).
- **GATE-12** Coverage floor per package. Total is 27.5 % per `cover.out`,
  which may be stale.
- **GATE-13** JSAdmission integration test checks no status fields, only that
  the reconcile succeeds.

## DOC: Documentation

- **DOC-1** README is the Kubebuilder template with `TODO(user)` placeholders.
- **DOC-2** "Synchronization" is shell-operator jargon, explained nowhere a
  user looks (README, CRD description, sample).
- **DOC-3** Move the generic Kubebuilder part of `AGENTS.md` into a
  `kubebuilder-scaffold` block. Write the planned blocks: js-sources,
  hook-dispatch, admission-webhook, kube-access, api-design, testing.
