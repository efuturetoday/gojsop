---
id: jsadmission
status: proposed
---

# JSAdmission

This area describes what a policy author can do with a `JSAdmission`: write the decision about a Kubernetes request in JavaScript and have the cluster enforce it.

The user is an engineer who writes a policy. They declare which requests the policy is for (`rules`, selectors, `matchPolicy`), put the script in a source, and export `validate(req)` for a validating policy or `mutate(req)` for a mutating one. gojsop calls the function for each matching request on the apiserver path and turns the return value into the answer of an admission webhook. The author also chooses what happens when the script fails (`failurePolicy`) and how long it may run (`timeoutSeconds`).

A policy decides on a request and must not change the cluster, so it can read other objects through `kube.get` and `kube.list` but cannot write. How the script is loaded and kept alive is covered by the aspects below; `JSHook` is the neighbouring area for scripts that react to events and may write.

## Use cases

### jsadmission.UC1 Reject requests that break a rule

- **Actor**: policy author
- **Trigger**: the author creates or changes a validating `JSAdmission`
- **Before**: the script exports `validate(req)`; the policy's `rules` match the kind and operation to guard
- **Steps**:
  1. The author applies the `JSAdmission`.
  2. A user creates or updates a matching object.
  3. The script receives `req` and returns `{allowed: false, message, code?, warnings?}` or `{allowed: true}`.
  4. The apiserver rejects or admits the request and shows the message and warnings to the user.
- **Exceptions**: a script that omits `allowed` denies (jsadmission.R4); a script error follows `failurePolicy` (jsadmission.R9); a modified object is ignored (jsadmission.R7)
- **Result**: only requests the script allowed are stored

### jsadmission.UC2 Change requests on their way in

- **Actor**: policy author
- **Trigger**: the author creates or changes a mutating `JSAdmission`
- **Before**: the script exports `mutate(req)`
- **Steps**:
  1. A user creates or updates a matching object.
  2. The script changes `req.object` and returns `{allowed: true, modifiedObject}`.
  3. gojsop turns the difference into a JSON patch and the apiserver applies it.
- **Exceptions**: server-managed fields in the change are dropped (jsadmission.R6); a difference that cannot be computed admits the request with a warning (jsadmission.R8); a script error follows `failurePolicy` (jsadmission.R9)
- **Result**: the stored object carries the change

### jsadmission.UC3 Decide with the context of the request

- **Actor**: policy author
- **Trigger**: the script runs for a request
- **Before**: none
- **Steps**:
  1. The script reads `req` (operation, kind, resource, name, namespace, user, `object`, `oldObject`, `dryRun`) (jsadmission.R3).
  2. The script looks up related objects with `kube.get` or `kube.list`.
  3. The script returns its decision.
- **Exceptions**: `kube.apply` and `kube.delete` do not exist for admission (jsadmission.R13)
- **Result**: the decision may depend on the request and on other objects, and never changes the cluster

### jsadmission.UC4 Choose what a failing policy means

- **Actor**: policy author
- **Trigger**: the script throws, runs out of memory, crashes or runs longer than `timeoutSeconds`, or the operator cannot be reached
- **Before**: the author set `failurePolicy` (`Fail` or `Ignore`) and optionally `timeoutSeconds`
- **Steps**:
  1. The request cannot get a decision from the script.
  2. With `Ignore` the request is admitted with a warning; otherwise it is denied with code 500.
  3. After a crash, memory overrun or timeout the script instance is rebuilt in the background; until it is ready `failurePolicy` decides every request (jsadmission.R19), and the policy shows `Ready=False` (jsadmission.R18).
- **Exceptions**: a plain script error does not restart the instance (jsadmission.R12)
- **Result**: the author gets the availability or the enforcement they chose

### jsadmission.UC5 Change or remove a policy

- **Actor**: policy author
- **Trigger**: the author edits or deletes the `JSAdmission`
- **Before**: the policy exists
- **Steps**:
  1. The author changes `rules`, selectors, `failurePolicy`, `timeoutSeconds` or the source, or deletes the policy.
  2. Requests that no longer match stop reaching the script; a deleted policy stops receiving any request.
- **Exceptions**: none
- **Result**: the cluster enforces the current policy set

## Rules

| ID | Rule | Source | Held by |
|---|---|---|---|
| jsadmission.R1 | Only requests that match the policy's `rules`, selectors and `matchPolicy` reach the script; a changed or deleted policy changes or ends that at once. | `api/v1alpha1/jsadmission_types.go` (`JSAdmissionSpec`) | `TestRegistrar_Update_OverwritesEntry`, `TestRegistrar_RemoveLastEntry_DeletesConfig` |
| jsadmission.R2 | A validating policy runs `validate(req)` and a mutating one `mutate(req)`; a missing function is a script failure. | `internal/jsadmission/handle.go` (`Handle`) | `TestHandle_Validate_Allow`, `TestHandle_MissingExport` |
| jsadmission.R3 | The script receives `uid`, `kind`, `resource`, `subResource`, `name`, `namespace`, `operation`, `userInfo`, `object`, `oldObject` and `dryRun`, with `object` and `oldObject` as real objects. | `internal/jsadmission/handle.go` (`AdmissionRequest`) | missing → ADM-9 |
| jsadmission.R4 | A script that omits `allowed` denies; a return of `undefined` or `null` is a script failure. | `internal/jsadmission/handle.go` (`AdmissionResult`, `Handle`) | missing → ADM-10 |
| jsadmission.R5 | `message`, `code` and `warnings` of the result reach the apiserver. | `internal/jsadmission/handle.go` (`AdmissionResult`) | `TestHandle_Validate_Deny` |
| jsadmission.R6 | For a mutating policy `modifiedObject` becomes a JSON patch against the original object; server-managed fields and `/status` are never in the patch. | `internal/jsadmission/diff.go` (`CreatePatch`) | `TestCreatePatch_FiltersImmutable`, `TestServer_Mutate_AddsLabel_AsJSONPatch` |
| jsadmission.R7 | A `modifiedObject` returned by a validating policy is ignored. | `AdmissionResult.ModifiedObject` comment in `handle.go` | missing → ADM-11 |
| jsadmission.R8 | When the patch cannot be computed the request stays allowed and gets a warning. | `fillResponse` in `server.go`; reason open in ADM-8 | missing → ADM-8 |
| jsadmission.R9 | On every script failure `failurePolicy` decides: `Ignore` allows with a warning, anything else denies with code 500. | `api/v1alpha1/jsadmission_types.go` (`FailurePolicy`) | `TestServer_Validate_FailurePolicy_Fail_OnJSThrow`, `TestServer_Validate_FailurePolicy_Ignore_OnJSThrow` |
| jsadmission.R10 | `failurePolicy` and `timeoutSeconds` are the same for the apiserver (operator unreachable) and for the handler. | `Registrar` and `PolicyEntry` in `internal/jsadmission` | missing → GATE-17 |
| jsadmission.R11 | A call that runs longer than the timeout (default 5 s) is a script failure. | `api/v1alpha1/jsadmission_types.go` (`TimeoutSeconds`); which field wins is open in ADM-5 | missing → GATE-22 |
| jsadmission.R12 | After a panic, a memory overrun or a timeout the script instance is rebuilt in the background; after a plain script error or a client disconnect it is kept. | `Server.review` in `server.go` | missing → GATE-22 |
| jsadmission.R13 | A policy can only read the cluster (`kube.get`, `kube.list`); `kube.apply` and `kube.delete` are not available. | admission runs with `sideEffects: None` | `TestSharedFactory_ForAdmission_ReadOnlySurface` |
| jsadmission.R14 | Every response carries the UID of its request. | admission.k8s.io/v1 | `TestServer_Validate_AllowedRoundtrip` |
| jsadmission.R15 | Requests in the operator's own namespace never reach a policy. | `cmd/main.go` (`excludeNamespaces`); extent open in ADM-6 | missing → ADM-6 |
| jsadmission.R16 | A response with `allowed=false` carries no patch. | admission.k8s.io/v1 (`AdmissionResponse`) | `TestServer_Mutate_Denied_HasNoPatch` |
| jsadmission.R17 | When the central webhook configurations cannot be written, the policy shows `Ready=False` with reason `WebhookSyncFailed`, the registrar retries, and `Ready` returns to `True` once it succeeds. | `Registrar.SyncError` and `JSAdmissionReconciler.Reconcile`; status-conditions.R1 | `TestReconcile_RegistrarSyncFailure_ShowsReadyFalse`, `TestRegistrar_SyncFailure_IsRetriedAndReported` |
| jsadmission.R18 | While the VM is not ready the policy is `Ready=False` with reason `Building` (the build runs; the reconcile does not wait for it) or `BuildFailed` (the last build failed; retried with backoff, a source change rebuilds at once). | [js-registry](../aspects/js-registry.md), status-conditions.R7 | `TestReconcile_BuildStates_ShowBuildingThenBuildFailed` |
| jsadmission.R19 | While the policy has no VM (a restart builds, or the build failed) every request is decided by `failurePolicy` at once; the request does not wait for the build. | [js-registry](../aspects/js-registry.md), js-registry.R19, `Server.review` | `TestServer_NoVM_AppliesFailurePolicyAtOnce` |

## Aspects

- [js-execution](../aspects/js-execution.md): how script calls run, with deadline, memory limit and error classes
- [js-registry](../aspects/js-registry.md): one script instance per policy, its rebuild and rescue
- [js-sources](../aspects/js-sources.md): where the script comes from
- [kube-access](../aspects/kube-access.md): the `kube` object, read-only for admission
- [status-conditions](../aspects/status-conditions.md): how the policy reports its state

## Decisions

- **All validating policies become entries of one `ValidatingWebhookConfiguration` (`gojsop-validating`), all mutating ones of one `MutatingWebhookConfiguration` (`gojsop-mutating`).** Status: accepted (2026-05, project). Why: one object to own and clean up. Not taken: one configuration per policy; the reason is not recorded.
- **A debounced `jsadmission.Registrar` is the only writer of both configurations and deletes one when its entry set is empty.** Status: accepted (2026-05, project). Why: one owner of the aggregation.
- **The operator serves each policy under `/admission/validate/<ns|cluster>/<name>` and `/admission/mutate/...`; the path is built only by `jsadmission.PathFor` and parsed only by `jsadmission.keyFromPath`.** Status: accepted (2026-05, project). Why: registrar and server must agree on one format; a policy is looked up per request.
- **A policy is registered in `jsadmission.Server` before it is published to the `Registrar`.** Status: accepted (2026-05, project). Why: once the configuration points at the operator requests arrive, and an unknown path answers 404.
- **The webhook entry is named `<ns>-<name>.policies.gojsop.io` (cluster-scoped: `<name>.policies.gojsop.io`).** Status: accepted (2026-05, project). Why: unique per policy inside one configuration.
- **The operator serves the webhook over TLS with a cert-manager certificate, and the `Registrar` reads the CA bundle on every sync.** Status: accepted (2026-05, project). Why: without `--webhook-cert-path` controller-runtime self-signs and the apiserver rejects that certificate. Open: CA rotation reaches the configurations only on the next policy change (ADM-4).
- **Admission VMs are built only by `SharedFactory.ForAdmission`, and scripts run only through `jsrun.Runner.Invoke`.** Status: accepted (2026-05, project). Why: one place for the read-only surface and for lock, panic recovery and result classification (see aspects). Not taken: binding `kube.apply` and `kube.delete`, because admission runs under `sideEffects: None`.
- **After a timeout the VM is rescued instead of leaving the goroutine running.** Status: accepted (2026-05, project). Why: wazero gained context cancellation, and a stuck VM could not be rescued safely before. Not taken: leaving the goroutine running.
- **The scaffolded Kubebuilder webhook for the `JSAdmission` CRD is an empty stub and validates nothing.** Status: proposed. Open: ADM-7.

## Open

ADM-3, ADM-4, ADM-5, ADM-6, ADM-7, ADM-8, EXEC-2, EXEC-4, STAT-4, OPS-1, GATE-7, GATE-13, GATE-17, GATE-18, GATE-22, GATE-23, ADM-9, ADM-10, ADM-11
