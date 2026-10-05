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
| Operator UI | UI | none (area not decided, UI-1) |
| Maintainer workspace and CLI | WS | [workspace](areas/workspace.md) |

## EXEC: JS execution

- **EXEC-11** `decision` The shape of an entry point is now written down
  (js-execution.R18) but was never chosen: the engine finds a function only as
  a property of `globalThis`, so a top-level `const handle = ...` fails with
  "missing required export: handle()" although the function is there. Decide
  whether that stays, or whether the engine also resolves the identifier
  (which reaches `const`, `let` and `class`). Changing it means `glue.c` and a
  `make engine-wasm` rebuild. Done when the choice is recorded in
  js-execution.
- **EXEC-12** `decision` TypeScript support. A bundler would run before apply,
  outside the operator. Open: which output gojsop promises to accept.
  `esbuild --format=iife` wraps every top-level declaration in a closure, so
  no entry point survives on `globalThis` (js-execution.R18) unless the author
  assigns one explicitly. Done when the contract is recorded in js-execution
  and held by a test over real bundler output.

## REG: JS registry and restarts

- **REG-3** `gap` No finalizer on JSHook / JSAdmission. Deletion is seen via
  NotFound on the next reconcile, an in-flight call can outlive the CR, and a
  mid-build `GetOrLoad` can install a zombie VM (`registry.go:317-319`). RBAC
  already declares `jshooks/finalizers`.

## STAT: Status and conditions

- **STAT-2** `decision` Only one condition type `Ready`, `Ready=False` has the
  single reason `Failed`. Users must read `message` or Events to tell causes
  apart.
- **STAT-4** `gap` No runtime telemetry in status. `lastExecution` and
  `lastReview` were removed in c7d58ac; users cannot see whether `handle()` or
  `validate()` runs or how long it takes. Replaces the stale TODO.md item
  "JSAdmission lastReview only populated on failure". See also OPS-1.

## SRC: JS sources

- **SRC-1** `gap` No OCI loader. The `spec.source.oci` field was removed until
  one exists (js-sources.R6); adding it means the field and the loader together.
  Direction (2026-10, to refine with WS-9): pinned by digest only, no tags and
  no polling, so new code still arrives through a changed CR and passes the
  access check (kube-access.R13). Mainly for bundles above the inline and
  ConfigMap size limits.
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
- **SRC-7** `decision` Packages and imports, like Go modules. scrippy (the
  predecessor) bundled scripts with esbuild and allowed URL imports (for
  example from GitHub) under a per-CR `ModulesPolicy`: host allowlist, SRI
  pins (`sha256-…`), size limits, fetch timeout; no policy means no imports.
  To decide: whether foreign code may be fetched at runtime at all (security),
  URL modules versus OCI artifacts (SRC-1), bundling inside the operator
  versus before apply. Done when decided and recorded in js-sources.

## DISP: Hook dispatch and bindings

- **DISP-4** `bug` No leader-election awareness. On failover the new leader
  rebuilds informers and delivers every object again as an initial Added; a
  deletion in the gap is never seen.
- **DISP-10** `debt` Failed events are requeued with `AddRateLimited` and no
  retry cap; a poison event retries forever (`dispatcher.go`, `requeue`). The
  same holds for events of a hook that has no VM (`noVM`): they wait, folded
  per object, until the VM is back or the hook is dropped. Not a cap, so not
  narrowed.
- **DISP-13** `gap` A JSHook has no way to run on a schedule or once at
  startup. `spec.bindings` only watches resources; shell-operator's
  `schedule` and `onStartup` have no CRD equivalent yet. Decide whether
  gojsop needs them and, if so, how they look as CRD fields.
## ADM: Admission webhook

- **ADM-3** `gap` `ReinvocationPolicy` is never set. The `PolicyMeta` field
  exists, the controller does not fill it (`registrar.go:43,215`,
  `internal/jsadmission/controller/controller.go:227-238`).
- **ADM-7** `doc` The scaffolded webhook for the JSAdmission CRD is empty
  (`internal/jsadmission/webhook/v1alpha1/jsadmission_webhook.go:55-100`).
  Fill it (e.g. syntax check, SRC-3) or remove it.

- **ADM-8** `decision` When the patch diff fails, `fillResponse` allows the
  request. The reason is not recorded; confirm or deny instead.
- **ADM-13** `decision` Background scans of existing objects (deferred
  2026-10-05, to refine). Audit and Warn only see new requests; before
  switching a policy to Deny, its owner must know which objects already in
  the cluster it would deny. Requirement: the full list of affected objects,
  per object with its reason, readable by each team for its own namespaces,
  so teams can be told and migrate. Counts alone are useless. Direction:
  PolicyReports (`wgpolicyk8s.io`, one per namespace, as Kyverno writes
  them), not a sample in the policy status. Draft for the script: `req.object`
  and `req.namespace` from the live object, `operation: CREATE`, no
  `userInfo`, `req.background: true`; validating policies only, on by
  default, `spec.background: false` to opt out; scan on change and hourly;
  leader only, through the same runner and limits as requests. Open: who
  installs the PolicyReport CRD when Kyverno already owns it; load on large
  clusters; resolving `*` in rules. Mutating policies are out of scope: to
  change existing objects, use a JSHook (mutate-existing).

## KUBE: Kubernetes access from JS

- **KUBE-1** `debt` `kube.apply` is not server-side apply. It does Get, then
  Create or JSON merge patch (lists replaced). The Get/Create race returns
  AlreadyExists with no retry (`KubeHost.apply` in `internal/jsengine/kubehost/kubehost.go`).
- **KUBE-3** `gap` `kube.list` has no limit or pagination; an empty namespace on
  a namespaced kind lists cluster-wide (`KubeHost.list` in `kubehost.go`).
- **KUBE-5** `decision` Network access for scripts: `fetch()` to call outside
  services, for example "is this customer paid before they may deploy"
  (deferred 2026-10-05, after the alpha). Draft: off by default; a script
  declares `spec.network.hosts`, HTTPS only, checked on every request and
  redirect; private and link-local addresses blocked after DNS resolution
  (cloud metadata, the apiserver past RBAC); every call bounded by the call
  deadline and a response size cap; secrets through `kube.get` and
  `permissions`. Open: who may allow a host, since no RBAC verb covers it.
  Proposal: an admin allowlist in the operator (Helm value or flag) and the
  script's hosts must be a subset, checked by the access webhook. Adding the
  field later is not a breaking change, so no placeholder now (api-design.R6).
  Done when decided and recorded in kube-access.

## API: CRD API surface

- **API-1** `decision` Project rule: no field without implementation or a
  status that shows it is inactive. Applies to CRD fields and to the schema
  that the CRDs expose. Holds today.
  Gate: GATE-10.
- **API-2** `doc` CRD field docs are thin. `kubectl explain jshook.spec.source`
  does not say which sources work.
- **API-3** `doc` `gojsop.io/restart` annotation value semantics undocumented.
  Any new value restarts; no convention (timestamp, hash, UUID).
- **API-6** `debt` Scope mismatch: markers and CRDs say `scope=Cluster`
  (`api/v1alpha1/jshook_types.go:67`, `jsadmission_types.go:156`), `PROJECT`
  says `namespaced: true` for both kinds. Fix `PROJECT`.
- **API-8** `decision` Breaking-change policy on `v1alpha1` is unwritten. Working
  assumption in api-design: edit in place until `v1beta1`.
- **API-9** `decision` Namespaced and cluster-scoped kinds. scrippy (the
  predecessor) split `ScriptHook` (own namespace only) from
  `ClusterScriptHook` (anywhere), Kyverno style. gojsop has only
  cluster-scoped kinds; each runs as its own ServiceAccount (kube-access.R10). Decide whether gojsop
  needs the split. Done when the decision is recorded in api-design.

## OPS: Operations

- **OPS-1** `gap` No Prometheus metrics. `--metrics-bind-address` exists, but
  nothing registers handle count and duration, restarts by reason, admission
  latency, allow and deny counts.
- **OPS-3** `debt` Fixed 5 s `RequeueAfter` on the failure paths that are not
  builds: source load, kube host, config invalid, subscribe, webhook sync
  (`fail` in both controllers). A bad source URL and a transient API error get
  the same retry. Build failures already back off exponentially. Done when
  these paths back off too.
- **OPS-4** `gap` Release process built (release-please, image, Helm chart,
  `install.yaml`; kubebuilder-scaffold Decisions) but no release published
  yet. Done when v0.1.0 is out and installs from the chart.
- **OPS-5** `decision` Tracing. scrippy exported OpenTelemetry traces per
  hook call. Decide whether gojsop traces calls, and how that relates to
  events and metrics (OPS-1). Done when decided and recorded in an aspect.
- **OPS-6** `decision` Audit log. scrippy kept an audit trail of what hooks
  did to the cluster (`kube.apply`, `kube.delete`). Decide whether gojsop
  needs one and where it lives. Done when decided and recorded in an aspect.

## GATE: Gates and test infrastructure

- **GATE-1** Add `make gates`: one target that runs all gates, identical in CI.
- **GATE-7** Check that admission VMs are built with `Factory.ForAdmission`
  (no `kube.apply` or `kube.delete`).
- **GATE-8** Status rules: `Reason:` only from `internal/conditions`
  constants, `Status()` only in `*/controller/`. Both hold today, nothing
  enforces them.
- **GATE-10** Table test that lists every spec field with an owner (code path
  or status field) (API-1).
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
  digest, bad enum).
- **GATE-16** envtest for SourceLoadFailed (event, condition, recovery) and
  for a ConfigMap edit that prepares the script again.
- **GATE-17** Test for jsadmission.R10. The registrar's `timeoutSeconds` and
  `failurePolicy` are testable in envtest, but the handler's copy sits in
  `Server.policies` with no accessor.
  Done when a test shows both sides get the same `failurePolicy` and the
  handler's timeout is not longer than the registrar's.
- **GATE-18** Webhook envtest suite lives in `internal/`
  (`internal/jsadmission/webhook/v1alpha1/webhook_suite_test.go:76`) and may
  skip silently without `KUBEBUILDER_ASSETS`. Move to `test/integration` or
  fail loudly.
- **GATE-19** CI checks for `go mod tidy` drift (`test.yml` runs it, never diffs)
  and `make lint-config`.
- **GATE-20** e2e hygiene: `make test-e2e` leaves the Kind cluster on failure
  (seen 2026-10-04: the cleanup runs only after a passing run). kind is pinned
  in CI since 2026-10-05.
- **GATE-22** Edge tests for `jsadmission.Server`: a timeout, a panic and the
  memory limit in `review` each apply `failurePolicy` and the next request
  runs on a fresh instance (jsadmission.R11, R12); bad requests in `serve`
  (405, 400, body over 3 MiB); `Registrar.mergeNSSelector`.
- **GATE-27** CI check that the committed `internal/jsengine/engine.wasm` equals a
  rebuild: run `make engine-wasm` and fail on a diff of `engine.wasm`. Needs the
  pinned toolchain of `glue/versions.env` on the runner (wasi-sdk 34, QuickJS-ng
  0.17.0, binaryen 129) and a check that the output is identical across macOS and
  Linux (the same build gave identical bytes twice on macOS arm64; Linux is
  untried). Rule: js-execution.R14.
- **GATE-28** e2e case with `replicas: 2` that shows a request is answered by
  the replica that is not the leader. The unit tests hold the shape of
  jsadmission.R20 (every replica registers, no status write, the option is
  set); only a real Service in front of two pods shows that the apiserver
  reaches both. Needs a Kind cluster with two ready manager pods and a way to
  address one pod directly. Rule: jsadmission.R20.
- **GATE-29** `gate` Report tests that no rule names (testing.R11). Today only the
  7 tests of the spike module `hack/spikes/quickjs-wasm` hold no rule. Done when: `make sdlc-check` or a test
  lists them and the list is empty or each entry is accepted on purpose.
- **GATE-30** `gate` CI fails when `dist/chart` differs from a fresh
  `make build-installer` plus `kubebuilder edit --plugins=helm/v2-alpha`. Seen
  2026-10-05: a CRD validation reached `config/crd` but not the chart.
  Done when the check runs in CI.

## DOC: Documentation

No open items.

## UI: Operator UI

- **UI-1** `decision` Operator web UI. scrippy had an Angular app (hook
  list and detail, live feed, audit overview, metrics panel, traces) with its
  own image. Decide whether gojsop gets a UI, for whom (operator admins, not
  hook authors), and what it shows. It would be a new area with its own
  users and a second side (front end). Depends on OPS-1, OPS-5, OPS-6 for
  its data. Done when decided; if yes, the area exists as `proposed`.

## WS: Maintainer workspace and CLI

Area: [workspace](areas/workspace.md). WS-1 to WS-4 became the area and
these steps (2026-10-05).

- **WS-5** `gap` Document the script contract (one ES2023 script, global
  entry point, no Node APIs, no async entry point) as a rule in
  js-execution, and have `gojsop build` check a JS file against it. Done
  when the rule exists and is held by a test.
- **WS-6** `gap` TypeScript: `gojsop build` bundles `.ts` with an embedded
  esbuild (or takes a ready `.js`), writes manifests with the inlined source
  to `dist/`, keeps source maps so errors point at `.ts` lines; `gojsop
  init` and `gojsop new hook|policy`; types as `gojsop.d.ts` and
  `@gojsop/types`. Done when a TypeScript workspace builds and tests.
- **WS-7** `gap` `gojsop serve --stdio` and `@gojsop/testing` (`runPolicy`,
  `runHook`) for vitest and jest; the npm package downloads the binary and
  keeps one engine process per test worker. Done when a vitest suite runs
  against the engine.
- **WS-8** `gap` Coverage: instrument the script with istanbul in
  `@gojsop/testing`, return the counters from the engine, merge them into
  the runner's report. Done when vitest shows coverage of a policy's lines.
- **WS-9** `decision` `gojsop push`: publish built manifests as an OCI
  artifact for Flux and Argo CD; the operator does not poll registries.
  Decide the artifact format and signing. Done when recorded.
