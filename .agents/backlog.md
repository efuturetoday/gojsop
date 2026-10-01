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
| JS execution | EXEC | [js-execution](aspects/js-execution.md) |
| JS registry and restarts | REG | [js-registry](aspects/js-registry.md) |
| Status and conditions | STAT | [status-conditions](aspects/status-conditions.md) |
| JS sources | SRC | [js-sources](aspects/js-sources.md) |
| Hook dispatch and bindings | DISP | [jshook](areas/jshook.md) |
| Admission webhook | ADM | [jsadmission](areas/jsadmission.md) |
| Kubernetes access from JS | KUBE | [kube-access](aspects/kube-access.md) |
| CRD API surface | API | [api-design](aspects/api-design.md) |
| Operations (metrics, RBAC, reconcile) | OPS | planned |
| Gates and test infrastructure | GATE | [testing](aspects/testing.md) |
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

- **REG-7** `bug` `GetOrLoad` rebuilds only on a `SourceHash` change. A changed
  `spec.limits` or other `BuildOptions` field does not rebuild the VM
  (`internal/jsregistry/registry.go:233,246`). Not verified at runtime.
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

- **SRC-4** `debt` ConfigMap mapper lists all CRs on every ConfigMap event (no
  field index, no predicate); a List error drops the event silently
  (`internal/jssource/watch.go:49-54`).
- **SRC-5** `decision` On SourceLoadFailed the old VM keeps serving the previous
  source (`internal/jshook/controller/controller.go:154`, inferred, untested).
  Keep or drop the VM.
- **SRC-6** `decision` SourceLoadFailed event message is fixed ("source loader
  failed"); the cause is only in the condition and the log
  (`internal/jshook/controller/controller.go:155`). This follows js-sources
  R5 (finite event messages). Confirm, or add a sanitized cause.
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

- **DISP-8** `bug` `Subscribe` holds `d.mu` through `WaitForCacheSync`. One slow
  informer blocks Subscribe and Drop for all hooks
  (`internal/jshook/dispatcher/dispatcher.go:109,307`).
- **DISP-9** `bug` Rescue rebuilds the VM, but the subscription keeps the
  `config()` result from Subscribe time. A changed config after rebuild is not
  applied (`dispatcher.go:523`, `internal/jshook/controller/controller.go:224`).
- **DISP-10** `debt` Failed events are requeued with `AddRateLimited` and no
  retry cap; a poison event retries forever (`dispatcher.go:547`).
- **DISP-11** `gap` Selectors only partly applied: top-level `nameSelector`,
  `fieldSelector`, `namespace.labelSelector`, `matchExpressions` and
  multi-namespace are ignored (`dispatcher.go:208-224`).
## ADM: Admission webhook

- **ADM-1** `bug` Registrar sync failure is invisible. CA read or API write
  errors are only logged, no retry, the CR still shows `Ready=True`
  (`internal/jsadmission/registrar.go:125-128`).
- **ADM-2** `bug` A denied mutation still sends a patch; `fillResponse` builds it
  even when `allowed=false` (`internal/jsadmission/server.go:337-358`).
- **ADM-3** `gap` `ReinvocationPolicy` is never set. The `PolicyMeta` field
  exists, the controller does not fill it (`registrar.go:43,215`,
  `internal/jsadmission/controller/controller.go:227-238`).
- **ADM-4** `debt` cert-manager CA rotation reaches the webhook configs only on
  the next policy change, contrary to the comment (`cmd/main.go:84-85`).
- **ADM-5** `bug` Admission uses `spec.timeoutSeconds` (default 5 s), not
  `spec.limits.timeoutSeconds` (`server.go:241-245`,
  `controller.go:138,195`). Not verified; decide which field wins.
- **ADM-6** `decision` `ExcludeNamespaces` holds only the operator namespace,
  although the registrar comment names kube-system and cert-manager
  (`cmd/main.go:282`, `registrar.go:59-62`).
- **ADM-7** `doc` The scaffolded webhook for the JSAdmission CRD is empty
  (`internal/jsadmission/webhook/v1alpha1/jsadmission_webhook.go:55-100`).
  Fill it (e.g. syntax check, SRC-3) or remove it.

- **ADM-8** `decision` When the patch diff fails, `fillResponse` allows the
  request. The reason is not recorded; confirm or deny instead.
- **ADM-9** `gate` No test that the script receives every field of the
  request (`uid`, `kind`, `resource`, `oldObject`, `userInfo`, `dryRun`, …)
  through the server. Done when a server test asserts each field
  (jsadmission.R3).
- **ADM-10** `gate` No test that an omitted `allowed` denies, or that a `null`
  or `undefined` return is a script failure. Done when both cases are tested
  (jsadmission.R4).
- **ADM-11** `gate` No test that a validating policy's `modifiedObject` is
  ignored. Done when a server test shows no patch for a validating policy
  (jsadmission.R7).
## KUBE: Kubernetes access from JS

- **KUBE-1** `debt` `kube.apply` is not server-side apply. It does Get, then
  Create or JSON merge patch (lists replaced). The Get/Create race returns
  AlreadyExists with no retry (`internal/jsengine/kubehost/kubehost.go:136-158`).
- **KUBE-2** `gap` `Factory` ignores `ctx`, `key` and `sa`; both callers pass
  `sa=""`. No per-hook identity (`kubehost/factory.go:46,53`,
  `internal/jshook/controller/controller.go:163`). Prerequisite for OPS-2.
- **KUBE-3** `gap` `kube.list` has no limit or pagination; an empty namespace on
  a namespaced kind lists cluster-wide (`kubehost.go:201`).
- **KUBE-4** `bug` A nil kube factory on a reconciler gives a VM without `kube`
  global and no error (`internal/jshook/controller/controller.go:162`,
  `internal/jsadmission/controller/controller.go:142`). Not verified.

## API: CRD API surface

- **API-1** `decision` Project rule: no field without implementation or a
  status that shows it is inactive. Applies to CRD fields and to the schema
  that JS `config()` returns. Today violated by SRC-1 (CRD field `oci`) and by
  DISP-1, DISP-2, DISP-3 (`config()` fields `schedule`, `jqFilter`, `queue`,
  `allowFailure`; the JSHook spec itself has only `source` and `limits`,
  `api/v1alpha1/jshook_types.go:24-32`). Gate: GATE-10.
- **API-2** `doc` CRD field docs are thin. `kubectl explain jshook.spec.source`
  does not say which sources work.
- **API-3** `doc` `gojsop.io/restart` annotation value semantics undocumented.
  Any new value restarts; no convention (timestamp, hash, UUID).
- **API-4** `debt` `config/samples/core_v1alpha1_jshook.yaml` uses fields without
  effect (schedule, jqFilter, queue, allowFailure). Samples should match what
  works.
- **API-5** `debt` `RestartReason` constants and the CRD doc list
  (`api/v1alpha1/js_shared.go:118`) can drift. Use a kubebuilder `Enum`
  marker. Gate: GATE-11.

- **API-6** `debt` Scope mismatch: markers and CRDs say `scope=Cluster`
  (`api/v1alpha1/jshook_types.go:67`, `jsadmission_types.go:156`), `PROJECT`
  says `namespaced: true` for both kinds. Fix `PROJECT`.
- **API-7** `gap` No `MaxLength` or `MaxItems` anywhere. `spec.source.inline` is
  unbounded against the etcd object size limit (`api/v1alpha1/js_shared.go:34-35`).
- **API-8** `decision` Breaking-change policy on `v1alpha1` is unwritten. Working
  assumption in api-design: edit in place until `v1beta1`.
## OPS: Operations

- **OPS-1** `gap` No Prometheus metrics. `--metrics-bind-address` exists, but
  nothing registers handle count and duration, restarts by reason, admission
  latency, allow and deny counts.
- **OPS-2** `gap` No per-hook RBAC narrowing. Wildcard `groups=*,resources=*`
  (`internal/jshook/controller/controller.go:103`); a bad script can touch
  anything.
- **OPS-3** `debt` Fixed 5 s `RequeueAfter` on every failure path. A bad
  source URL and a transient API error get the same retry.
- **OPS-4** `gap` No release process. No tags, no versioned image.

## GATE: Gates and test infrastructure

- **GATE-1** Add `make gates`: one target that runs all gates, identical in CI.
- **GATE-2** Import and access boundaries via golangci `depguard` or an arch
  test: no `fastschema/qjs` or `wazero` outside `internal/jsengine/**`;
  `jsengine` used only by `jsregistry`, `jshook`, `jsadmission`; `jsregistry`
  does not import `jssource`; `ManagedVM.VM` and `ManagedVM.CallMu` are not
  touched outside `jsregistry` (needs a `go/analysis` check). Blocked by EXEC-1.
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
- **GATE-12** Coverage floor per package. Unit run 2026-10-01: conditions 100,
  jssource 88.1, jshook 75.0, kubehost 67.5, jsregistry 67.4, jsadmission
  66.2, jsengine 49.3, jslifecycle 36.4; 0 for both controllers, the
  dispatcher and the webhook package.
- **GATE-13** JSAdmission integration test checks no status fields, only that
  the reconcile succeeds.
- **GATE-14** CI fails when `make manifests generate` leaves a diff
  (generated files not committed). Covers `config/rbac/role.yaml` sync with
  RBAC markers.

- **GATE-15** envtest cases for CEL and enum rejection (two sources, tag plus
  digest, bad enum) and a check that `config/samples` apply.
- **GATE-16** envtest for SourceLoadFailed (event, condition, recovery), for an
  `oci` source (must yield SourceLoadFailed) and for a ConfigMap edit that
  rebuilds the VM.
- **GATE-17** Tests for both controllers and the dispatcher. No test imports
  `internal/jshook/dispatcher`; sync ordering, filters, timeout streak and
  requeue are untested.
- **GATE-18** Webhook envtest suite lives in `internal/`
  (`internal/jsadmission/webhook/v1alpha1/webhook_suite_test.go:76`) and may
  skip silently without `KUBEBUILDER_ASSETS`. Move to `test/integration` or
  fail loudly.
- **GATE-19** CI checks for `go mod tidy` drift (`test.yml` runs it, never diffs)
  and `make lint-config`.
- **GATE-20** e2e hygiene: `make test-e2e` leaves the Kind cluster on failure
  (`Makefile:89-92`, unverified); `test-e2e.yml:20` installs kind unpinned.
- **GATE-22** Edge tests for `jsadmission.Server`: timeout, panic and memory
  limit in `review` (with rescue), bad requests in `serve` (405, 400, body
  over 3 MiB), and `Registrar.mergeNSSelector`.
- **GATE-23** e2e case that runs a JSAdmission against a real apiserver over
  TLS with cert-manager.
- **GATE-24** Test that a `kube.*` call from JS ends at the script deadline
  (needs EXEC-2).
## DOC: Documentation

- **DOC-1** README is the Kubebuilder template with `TODO(user)` placeholders.
- **DOC-2** "Synchronization" is shell-operator jargon, explained nowhere a
  user looks (README, CRD description, sample).
- **DOC-4** Stale comments beyond STAT-3: `status.lastExecution.error` in
  `internal/jshook/dispatcher/dispatcher.go:495`; `BindingContext.Type` lists
  "Schedule", never produced (`internal/jshook/bindingctx.go:11`); "MVP wires
  only inline" and an old restart trigger in `internal/jssource/loader.go:21,41-42`;
  `kubehost.FieldManager` calls itself a server-side-apply field manager
  (KUBE-1).
- **GATE-25** `gate` CI cannot fetch the private module
  `github.com/efuturetoday/agentic-sdlc` yet. `make sdlc-check` in
  `lint.yml` needs `GOPRIVATE` plus a token with read access, set as a
  repository secret once gojsop has a remote. Done when the lint job runs
  the SDLC check green.
