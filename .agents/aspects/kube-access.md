---
id: kube-access
status: accepted
entrypoints:
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
`ForHook` returns the full surface, `ForAdmission` the read-only one. Today's
only implementation, `kubehost.SharedFactory`, hands out binders over one
process-wide dynamic client and RESTMapper. It ignores its context, key and
ServiceAccount arguments. The factory replaced a process-wide singleton field
so that a per-ServiceAccount implementation (TokenRequest) can be swapped in
later. The dynamic client with a RESTMapper fits the free-form `apiVersion` and
`kind` that scripts pass. Why it won over typed clients is not recorded.

There is no per-hook identity. The operator's own ServiceAccount performs every
call, and its ClusterRole is a wildcard. The code calls this an MVP; a per-hook
ServiceAccount and narrower RBAC are planned as "Phase 2". A reconciler without
a factory builds a VM without `kube`, and no error is raised.

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
  Why: one seam for swapping in per-ServiceAccount clients later.
  Gate: `TestSharedFactory_ForHook_FullSurface`, `TestSharedFactory_PerCallInstances`.
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
- **R7** Do not read `kubehost.KubeHost` fields from another goroutine.
  Why: calls arrive on the per-hook worker goroutine, and the registry
  serialises calls per VM (held by js-registry.R8), so the type has no locking.
  Gate: review only — no machine can tell which goroutine reads a field.
- **R8** Wire a `kubehost.Factory` into every reconciler that builds a VM —
  `JSHookReconciler` and `JSAdmissionServerReconciler`; `SetupWithManager`
  fails without one. The leader-only `JSAdmissionReconciler` builds nothing
  (jsadmission.R20) and needs no factory.
  Why: without a factory every script runs without the `kube` global and no
  error says why. Only a reconciler built bare in a unit test runs without it.
  Gate: `TestSetupWithManager_RequiresKubeHost` (jshook and jsadmission controllers).

## Decisions

- **Scripts reach the cluster through a dynamic client with a RESTMapper.**
  Status: accepted (date and approver not recorded; migrated from block).
  Why: it fits the free-form `apiVersion` and `kind` that scripts pass. Not
  taken: typed clients; the reason is not recorded.
- **Binders come from a `kubehost.Factory`, not a process-wide singleton.**
  Status: accepted (date and approver not recorded; migrated from block).
  Why: a per-ServiceAccount implementation (TokenRequest) can be swapped in
  later. Not taken: the singleton field, because it blocks that swap.
- **The operator's own ServiceAccount performs every call (MVP).**
  Status: accepted (date and approver not recorded; migrated from block).
  Why: per-hook identity and narrower RBAC are planned as "Phase 2" (OPS-2).
  Not taken: per-hook ServiceAccounts now, because they need TokenRequest work.
- **`kube.apply` is Get plus Create or JSON merge patch.**
  Status: accepted (date and approver not recorded; migrated from block).
  Why: server-side apply was deferred; Phase 2 may switch once only modern API
  servers are supported (KUBE-1). Not taken: server-side apply, because it was
  deferred.

## Open

Tracked in [backlog](../backlog.md): KUBE-1 to KUBE-3, OPS-2; gates GATE-7, GATE-14.
