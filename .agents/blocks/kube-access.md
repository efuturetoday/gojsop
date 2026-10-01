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

This block describes how user JavaScript reaches the Kubernetes API.

Scripts see one global object, `kube`. Hooks get `apply`, `get`, `list` and
`delete`. Admission policies get `get` and `list`; `apply` and `delete` are
`undefined` for them. The [js-execution](js-execution.md) block explains where
`kube` sits in the VM.

A `kubehost.Factory` mints the binder that installs `kube` into a VM.
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
- `kube.get` returns `null` for a missing object and throws on other errors.
- `kube.list` issues one List without limit or pagination. An empty namespace on
  a namespaced kind lists across all namespaces.
- `kube.delete` treats NotFound as success and uses default delete options.

An unknown kind fails with a RESTMapping error. Whether the manager's mapper
sees CRDs installed after start is not verified.

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
  Violated today → KUBE-1 (the `FieldManager` doc comment still says server-side apply).
- **R5** Bound every `kube.*` call by the deadline of the running script call.
  Why: R3 of [js-execution](js-execution.md) promises a deadline for the whole call.
  Gate: missing → GATE-24. Violated today → EXEC-2.
- **R6** Put the `+kubebuilder:rbac` marker next to the code that needs the
  permission and run `make manifests`. `config/rbac/role.yaml` is generated.
  Why: the generated role must not drift from the code.
  Gate: missing → GATE-14.
- **R7** Do not read `kubehost.KubeHost` fields from another goroutine.
  Why: calls arrive on the per-hook worker goroutine, and the registry
  serialises calls per VM, so the type has no locking.
  Gate: missing → GATE-3 (`-race`).

## Rejected

- Typed clients instead of dynamic client plus RESTMapper: reason not recorded.
- Server-side apply for `kube.apply`: deferred to Phase 2 (KUBE-1).

## Open

Tracked in [backlog](../backlog.md): KUBE-1 to KUBE-4, EXEC-2, OPS-2; gates GATE-3, GATE-7, GATE-14, GATE-24.
