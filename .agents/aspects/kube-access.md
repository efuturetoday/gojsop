---
id: kube-access
status: accepted
entrypoints:
  - jsaccess.Manager.Ensure
  - jsaccess.Checker.Handle
  - kubehost.Factory
  - kubehost.SharedFactory.ForHook
  - kubehost.KubeHost.Bind
  - kubehost.ReadOnlyKubeHost.Bind
---

# Kube Access

This aspect describes how user JavaScript reaches the Kubernetes API.

Scripts see one global object, `kube`. Hooks get `apply`, `get`, `list` and
`delete`. Admission policies get `get` and `list`; `apply` and `delete` are
`undefined` for them. The [js-execution](js-execution.md) aspect explains where
`kube` sits in the VM.

A `kubehost.Factory` mints the binder that registers `kube` for a VM. The
functions cross into wasm through the one import `env.host_call`; the engine
defines `kube.get` and the others as JavaScript functions over it, and only
registered names exist.
`ForHook` returns the full surface, `ForAdmission` the read-only one. The only
implementation, `kubehost.SharedFactory`, shares one RESTMapper and hands each
binder a dynamic client that acts as the ServiceAccount it was minted for. The
dynamic client with a RESTMapper fits the free-form `apiVersion` and `kind`
that scripts pass. Why it won over typed clients is not recorded.

**Every hook and policy has its own identity.** `spec.permissions` lists what
the script may do. `jsaccess.Manager` gives each JSHook and JSAdmission a
ServiceAccount of its own in the operator's namespace, a ClusterRole with
exactly those rights (for a hook plus `get`, `list`, `watch` on what its
bindings watch) and a ClusterRoleBinding; all three are owned by the resource
and go with it. A hook's watches and every `kube.*` call run as that
ServiceAccount, through impersonation. A script without a right gets
`Forbidden`, as a catchable exception.

**Nobody hands out a right they do not hold.** The validating webhook
`jsaccess.Checker` admits a JSHook or JSAdmission only when the user who
creates or changes it holds every right its ServiceAccount would get; it asks
the apiserver with a SubjectAccessReview per verb and resource. A change of
metadata alone (labels, the restart annotation) is not checked. The one gap is
a source in a ConfigMap: whoever may edit that ConfigMap changes the script and
uses the hook's rights without passing the check. That is documented, not
closed.

The operator itself keeps what it needs for that: its own CRDs, webhook
configurations, ConfigMaps and Namespaces to read, ClusterRoles and
ClusterRoleBindings with `escalate` and `bind`, and ServiceAccounts with
`impersonate` in its own namespace only. Whoever controls the operator can
still grant any right; the scripts cannot.

Semantics of the four functions, each taking one JS object:

- `kube.apply` is not server-side apply, despite the name. It reads the object,
  creates it if missing, otherwise sends a JSON merge patch with the field
  manager `gojsop`. Lists are replaced, and removing a key needs `null`. A
  concurrent creator makes it fail with AlreadyExists; there is no retry.
  Server-side apply was deferred; the code says Phase 2 may switch once only
  modern API servers are supported.
- `kube.get` returns `null` for a missing object and throws on other errors. A
  failed call is a JavaScript exception with the error text; a script may
  catch it.
- `kube.list` issues one List without limit or pagination. An empty namespace on
  a namespaced kind lists across all namespaces.
- `kube.delete` treats NotFound as success and uses default delete options.

An unknown kind fails with a RESTMapping error. Whether the manager's mapper
sees CRDs installed after start is not verified.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once? | `kubehost.Factory`, with `kubehost.SharedFactory` as the only implementation, binds `kubehost.KubeHost` (full) or `kubehost.ReadOnlyKubeHost` into a VM. Searched `internal` and `cmd` for other uses of `dynamic` clients: only `cmd/main.go` builds one. |
| example | Which real use should others copy? | `jshook` controller takes a `kubehost.Factory` and calls `ForHook`; the admission controller does the same with `ForAdmission`. |
| test helper | How does a test use the aspect without effort? | `newKubeHost` in the kubehost tests builds a `kubehost.KubeHost` over a fake dynamic client and a test RESTMapper. |
| sides | Which sides does it touch? | Back end only: operator process, Kubernetes API, and the generated RBAC role. |
| tie | How do the sides stay in step? | n/a, because one side runs the code. The RBAC role is generated from markers (R6); no test checks it yet, see GATE-14. |

## How to use it

1. Add the function to `kubehost.KubeHost` as a `jsengine.HostFunc` (JSON in, JSON out, the call context first) and register it in `Bind` with `Host.Func("kube.<name>", ...)`.
2. If it cannot write, bind it in `kubehost.ReadOnlyKubeHost.Bind` too.
3. Add a `TestKubeHost_*` test and put `// kube-access.R3` above it.
4. If it needs a new permission, add the `+kubebuilder:rbac` marker and run `make manifests`.
5. Run `make test lint`.

## Rules

- **R1** Reach the cluster from JavaScript only through `kube`, and obtain its
  binder only from a `kubehost.Factory`. Build the dynamic client once, in
  `cmd/main.go`.
  Why: one seam where every binder gets the client of its ServiceAccount (R12).
  Gate: `TestSharedFactory_ForHook_FullSurface`, `TestSharedFactory_PerCallInstances`, `TestSharedFactory_BothSurfaces_ShareClientAndMapper`.
- **R2** Bind only `get` and `list` for admission VMs.
  Why: the admission webhook declares `sideEffects: None`, and the API server
  may retry the request.
  Gate: `TestSharedFactory_ForAdmission_ReadOnlySurface`. Wiring check missing → GATE-7.
- **R3** Add a new `kube.*` function to `kubehost.KubeHost`. Bind it in
  `kubehost.ReadOnlyKubeHost` too only if it cannot write.
  Why: the read-only binder is a thin wrapper over the same client.
  Gate: `TestKubeHost_*`.
- **R4** Do not treat `kube.apply` as server-side apply, and do not rely on the
  field manager for conflict handling.
  Why: it is Get plus Create or merge patch; no field ownership is tracked.
  Gate: `TestKubeHost_ApplyCreatesAndUpdates`, `TestKubeHost_RepeatedApplyInLoop`.
  Violated today → KUBE-1 (the `FieldManager` doc comment still says
  server-side apply).
- **R5** Bound every `kube.*` call by the deadline of the running script call.
  Why: R3 of [js-execution](js-execution.md) promises a deadline for the whole call.
  Gate: `TestKubeHost_CallIsBoundByJSCallDeadline`, `TestKubeHost_ParentContextEndsCall`.
  The engine hands the host function the context of the running call
  (js-execution.R11); `KubeHost.Ctx` (manager shutdown) ends a call too.
- **R6** Put the `+kubebuilder:rbac` marker next to the code that needs the
  permission and run `make manifests`. `config/rbac/role.yaml` is generated.
  Why: the generated role must not drift from the code.
  Gate: missing → GATE-14.
- **R7** Keep `kubehost.KubeHost` free of state that a call writes: its
  fields are set once at construction and only read.
  Why: calls of one script run in parallel (js-registry.R1) and share one
  `KubeHost`; it has no locking, and the dynamic client and the RESTMapper are
  safe for concurrent use.
  Gate: review only — no machine can tell a field written per call from one set once.
- **R8** Wire a `kubehost.Factory` into every reconciler that builds a VM —
  `JSHookReconciler` and `JSAdmissionServerReconciler`; `SetupWithManager`
  fails without one. The leader-only `JSAdmissionReconciler` builds nothing
  (jsadmission.R20) and needs no factory.
  Why: without a factory every script runs without the `kube` global and no
  error says why. Only a reconciler built bare in a unit test runs without it.
  Gate: `TestSetupWithManager_RequiresKubeHost` (jshook and jsadmission controllers).
- **R9** `kube.get` returns `null` for a missing object, `kube.list` returns
  the items, `kube.delete` removes the object.
  Why: scripts branch on a missing object without `try/catch`; authors rely
  on these semantics.
  Gate: `TestKubeHost_GetReturnsNullForMissing`, `TestKubeHost_ListReturnsItems`, `TestKubeHost_DeleteRemovesResource`.

- **R10** Give every JSHook and JSAdmission a ServiceAccount, ClusterRole and
  ClusterRoleBinding of its own, named `jshook-<name>` or `jsadmission-<name>`
  (cut and hashed when too long), owned by the resource, created before
  anything runs as it.
  Why: one identity per script is what lets its rights be narrow; the owner
  reference removes them with the resource.
  Gate: `TestName_PrefixesKindAndCutsLongNames`, `TestEnsure_CreatesServiceAccountRoleAndBindingOwnedByTheResource`; removal by the garbage collector on a real cluster: `TestE2E`.
- **R11** Give a hook exactly its `spec.permissions` plus `get`, `list` and
  `watch` on every resource its bindings watch; give a policy exactly its
  `spec.permissions`.
  Why: the watches run as the hook, so they need read rights; nothing else is
  granted that the resource does not say.
  Gate: `TestHookRules_AddReadOnEveryWatchedResource`.
- **R12** Run a hook's watches and every `kube.*` call as the resource's
  ServiceAccount, through a client that impersonates it; never through the
  operator's own client.
  Why: otherwise the narrow rights of R10 protect nothing.
  Gate: `TestImpersonatingConfig_ActsAsTheServiceAccount`, `TestSharedFactory_As_UsesTheServiceAccountsClient`, `TestDispatcher_WatchesRunAsTheHooksClient`, `TestE2E`.
- **R13** Admit a JSHook or JSAdmission only when the user who creates it, or
  changes its spec, holds every right of R11 cluster-wide. A change of
  metadata alone is not checked.
  Why: Kubernetes' own rule for Roles; without it writing a hook would be a
  way to any right the operator can grant.
  Gate: `TestChecker_DeniesRightsTheUserDoesNotHold`, `TestChecker_AllowsWhenTheUserHoldsEveryRight`, `TestChecker_UpdateOfTheScriptChecksEveryRight`, `TestE2E`.
- **R14** Name every API group and resource in `spec.permissions` explicitly:
  no `*`. A policy's permissions only read: `get`, `list`, `watch`.
  Why: a permission must say exactly what the script touches; admission runs
  with `sideEffects: None`.
  Gate: `TestControllers` (spec.permissions).

## Decisions

- **Scripts reach the cluster through a dynamic client with a RESTMapper.**
  Status: accepted (date and approver not recorded; migrated from block).
  Why: it fits the free-form `apiVersion` and `kind` that scripts pass. Not
  taken: typed clients; the reason is not recorded.
- **Binders come from a `kubehost.Factory`, not a process-wide singleton.**
  Status: accepted. Why: one place hands every binder the client of its
  ServiceAccount (R12). Not taken: the singleton field.
- **Every hook and policy runs as a ServiceAccount of its own, with the rights
  it declares in `spec.permissions`, checked against the user who writes it.**
  Status: accepted (2026-10, OPS-2, KUBE-2; replaces "the operator's own
  ServiceAccount performs every call").
  Why: with one wildcard identity, anyone who may write a JSHook could read
  every Secret of the cluster. Declaring the rights in the resource keeps
  everything in one place, and the check is Kubernetes' own rule for Roles.
  Not taken: the jsPolicy way (operator is `cluster-admin`, only admins write
  policies), because one careless script reaches everything. The Kyverno way
  (admins extend the operator's role by aggregation), because the rights are
  then shared by every hook and live apart from it. TokenRequest instead of
  impersonation, because it needs token refresh and gives nothing more.
- **A source in a ConfigMap is not checked against the hook's rights.**
  Status: accepted (2026-10). Why: the check runs when the hook is written; a
  ConfigMap edit never passes it, and watching every ConfigMap write costs
  more than it protects. The README says to protect the ConfigMap like the
  hook. Not taken: allowing ConfigMaps only from the operator's namespace,
  because teams could then not keep scripts in their own namespaces.
- **`kube.apply` is Get plus Create or JSON merge patch.**
  Status: accepted (date and approver not recorded; migrated from block).
  Why: server-side apply was deferred; Phase 2 may switch once only modern API
  servers are supported (KUBE-1). Not taken: server-side apply, because it was
  deferred.

## Open

Tracked in [backlog](../backlog.md): KUBE-1, KUBE-3; gates GATE-7, GATE-14.
