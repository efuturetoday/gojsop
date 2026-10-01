# Block: Kube Access

Status: accepted

## Decision

JS reaches the Kubernetes API only through the `kube` global, bound per VM by a `jsengine.HostBinder` that a `kubehost.Factory` mints. All binders today share one process-wide dynamic client and RESTMapper. There is no per-hook identity: the operator's own ServiceAccount does every call, and its ClusterRole is a wildcard (`internal/jshook/controller/controller.go:103`, generated into `config/rbac/role.yaml`). The comment at `controller.go:101-102` calls this an MVP, with a per-hook ServiceAccount planned as Phase 2.

`7a14471` replaced a process-wide singleton `KubeHost` field with the `Factory` interface so a later per-ServiceAccount implementation (TokenRequest) can be swapped in (`kubehost/factory.go:13-22`). Reason for the dynamic client plus RESTMapper instead of typed clients: not recorded; it fits free-form `apiVersion/kind` input from JS. Server-side apply was deferred: the comment at `kubehost.go:136-138` says "Phase 2 may switch" once only modern API servers are supported.

## Code

- Factory: interface `Factory{ForHook, ForAdmission}` (`kubehost/factory.go:23`). Only implementation `SharedFactory` (:31), built by `NewSharedFactory(ctx, dyn, mapper)` (:39). Both methods ignore their `ctx`, `key` and `sa` arguments (:46,53). Each call returns a fresh binder struct over the same client.
- Binders: `KubeHost` (`kubehost.go:24`) binds `apply/get/list/delete` (:52-63). `ReadOnlyKubeHost` (:75) embeds `*KubeHost` and binds only `get/list` (:80-89); `apply` and `delete` are `undefined` in JS.
- Consumers: `JSHookReconciler.KubeHost` calls `ForHook` (`internal/jshook/controller/controller.go:162`); `JSAdmissionReconciler.KubeHost` calls `ForAdmission` (`internal/jsadmission/controller/controller.go:143`). Both only bind when the field is non-nil (:162, :142). A nil factory yields a VM with no `kube` global, no error.
- Wiring: `cmd/main.go:235` builds `dynamic.NewForConfig(mgr.GetConfig())`. `cmd/main.go:247` builds one `SharedFactory` with `managerCtx` (the signal-handler context, :241) and `mgr.GetRESTMapper()`. The same instance goes to both reconcilers (:261, :290). The dispatcher gets the same dynamic client and mapper for its informers (:248), separate from the factory.
- Resolution: `resourceFor` (`kubehost.go:100`) parses `apiVersion`, calls `Mapper.RESTMapping(GroupKind, version)`, and adds `.Namespace(ns)` only for namespaced scope. An unknown kind fails with `RESTMapping <apiVersion>/<kind>: ...`. Whether the manager's mapper refreshes for CRDs installed after start: unsure, not verified here.
- Context: every call uses `h.callCtx()` (`kubehost.go:91`), which is `KubeHost.Ctx` = `managerCtx`, or `context.Background()` if nil. It is not the per-call deadline context passed to `Registry.Call`. `t.Context()` is used only to build JS values. This is EXEC-2.

Semantics of the four functions (all take one JS object):

- `kube.apply(obj)` (`:119`): needs `apiVersion`, `kind`, `metadata.name`. It is not server-side apply, despite the name. It does Get; on NotFound it Creates with `FieldManager: "gojsop"` (:17, :141); otherwise it copies the existing `resourceVersion` into the object and sends a JSON merge patch with the same field manager (:151-158). Returns the persisted object. Merge patch means lists are replaced and removing a key requires `null`. The Get-then-Create path is racy against a concurrent creator (AlreadyExists is returned as an error, no retry). Creating a namespaced object with empty `metadata.namespace` is not defaulted here; behaviour comes from the API server (not verified).
- `kube.get({apiVersion, kind, name, namespace})` (:166): returns `null` on NotFound (:180), throws on other errors.
- `kube.list({apiVersion, kind, namespace, labelSelector, fieldSelector})` (:189): one `List`, no limit or pagination, returns an array of plain objects. Empty namespace on a namespaced kind lists across all namespaces.
- `kube.delete(ref)` (:220): NotFound counts as success, returns `true`. Default `DeleteOptions` (no propagation policy, no preconditions).

## Rules

- Do: add new `kube.*` functions to `KubeHost`, and bind read-only ones in `ReadOnlyKubeHost.Bind` too only if they cannot write.
- Do: get binders only from a `kubehost.Factory`; construct the dynamic client once in `cmd/main.go`.
- Do: when adding RBAC, put the `+kubebuilder:rbac` marker next to the code that needs it and run `make manifests`; `config/rbac/role.yaml` is generated.
- Don't: bind `apply` or `delete` for admission VMs (webhook has `sideEffects: None`, `kubehost.go:65-69`).
- Don't: read a `KubeHost` field from another goroutine. The comment at :21-23 says calls arrive on the per-hook worker goroutine, `Registry.Call` also serialises per VM via `CallMu`.
- Don't: assume `kube.apply` is SSA or tracks field ownership; do not rely on `FieldManager` for conflict handling.

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| Full vs read-only surface, per-call instances | `go test ./internal/jsengine/kubehost -run 'TestSharedFactory_'` | yes (`make test` in `test.yml`) |
| apply creates then updates, repeated apply, get null, list, delete (fake dynamic client) | `go test ./internal/jsengine/kubehost -run 'TestKubeHost_'` (`ApplyCreatesAndUpdates`, `RepeatedApplyInLoop`, `GetReturnsNullForMissing`, `ListReturnsItems`, `DeleteRemovesResource`) | yes |
| `kube.*` bound by call-time deadline | missing: test with a blocking fake dynamic client and a short per-call context asserting the call returns `context.DeadlineExceeded` (needs EXEC-2 fix first) | missing |
| Admission VM built via `ForAdmission` | missing: see GATE-7 | missing |
| Error paths: unknown kind, missing fields, AlreadyExists race, namespaced kind without namespace | missing: table test in `kubehost_test.go` | missing |
| RBAC marker vs `config/rbac/role.yaml` in sync | missing: CI step running `make manifests` and `git diff --exit-code config/rbac` | missing |
| Real API server behaviour (RESTMapper, merge patch, CRDs) | missing: envtest case that applies a ConfigMap and a CRD object via a hook | missing |

Gates marked `missing` are commitments, not facts.

## Open

Tracked in [backlog](../backlog.md): KUBE-1 to KUBE-4, EXEC-2, OPS-2; gates GATE-7, GATE-14.
