---
id: jsadmission
status: proposed
---

# JSAdmission

This area describes what a policy author can do with a `JSAdmission`: decide on a Kubernetes
request in JavaScript and have the cluster enforce the decision.

The author declares which requests the policy is for (`rules`, selectors,
`matchPolicy`) and writes a script that defines `validate(req)` for a
validating policy or `mutate(req)` for a mutating one. gojsop calls it for
each matching request and turns the return value into the webhook's answer.
The author also chooses what a failure means (`failurePolicy`) and how long a
call may run (`timeoutSeconds`).

A policy decides; it does not change the cluster. It can read with
`kube.get` and `kube.list`, never write. Scripts that react to events and may
write are the neighbour area [jshook](jshook.md).

Words used here:

- **script**: the policy's source, loaded and prepared by gojsop. A new
  script is prepared when the source, the limits or the restart annotation
  change.
- **call**: one run of `validate()` or `mutate()` for one request. Every call
  starts from the freshly prepared script; nothing a call leaves behind
  reaches the next one.

## Use cases

### jsadmission.UC1 Reject requests that break a rule

- **Actor**: policy author
- **Trigger**: applies a validating `JSAdmission`
- **Before**: the script defines `validate(req)`; `rules` match the kinds and operations to guard
- **Steps**:
  1. A user creates or updates a matching object.
  2. The script returns `{allowed: false, message, code?, warnings?}` or `{allowed: true}`.
  3. The apiserver rejects or admits the request and shows message and warnings.
- **Exceptions**: a result without `allowed` denies (jsadmission.R4); a failed call follows `failurePolicy` (jsadmission.R9); a returned `modifiedObject` is ignored (jsadmission.R7).
- **Result**: only requests the script allowed are stored.

### jsadmission.UC2 Change requests on their way in

- **Actor**: policy author
- **Trigger**: applies a mutating `JSAdmission`
- **Before**: the script defines `mutate(req)`
- **Steps**:
  1. A user creates or updates a matching object.
  2. The script changes `req.object` and returns `{allowed: true, modifiedObject}`.
  3. gojsop sends the difference as a JSON patch; the apiserver applies it.
- **Exceptions**: server-managed fields are dropped from the patch (jsadmission.R24); a patch that cannot be computed admits with a warning (jsadmission.R8); a failed call follows `failurePolicy` (jsadmission.R9).
- **Result**: the stored object carries the change.

### jsadmission.UC3 Decide with the context of the request

- **Actor**: policy author
- **Trigger**: a call runs
- **Steps**:
  1. The script reads `req`: operation, kind, resource, name, namespace, user, `object`, `oldObject`, `dryRun` (jsadmission.R3).
  2. The script reads related objects with `kube.get` or `kube.list`, as far as `spec.permissions` allows ([kube-access](../aspects/kube-access.md)).
  3. The script returns its decision.
- **Exceptions**: `kube.apply` and `kube.delete` do not exist (jsadmission.R13).
- **Result**: the decision may depend on the request and on other objects, and never changes the cluster.

### jsadmission.UC4 Choose what a failing policy means

- **Actor**: policy author
- **Trigger**: the call throws, hits the memory limit, crashes the engine or runs past `timeoutSeconds`; or no script is ready; or the operator cannot be reached
- **Before**: the author set `failurePolicy` (`Fail` or `Ignore`)
- **Steps**:
  1. With `Ignore` the request is admitted with a warning; otherwise it is denied with code 500.
  2. The next request starts from the prepared script; nothing is prepared again (jsadmission.R12).
- **Exceptions**: while a new script is prepared, `failurePolicy` decides every request at once (jsadmission.R19), and the policy shows `Ready=False` (jsadmission.R18, jsadmission.R22).
- **Result**: the author gets the availability or the enforcement they chose.

### jsadmission.UC5 Change or remove a policy

- **Actor**: policy author
- **Trigger**: edits or deletes the `JSAdmission`
- **Steps**:
  1. The author changes `rules`, selectors, `failurePolicy`, `timeoutSeconds` or the source, or deletes the policy.
  2. Requests that no longer match stop reaching the script; a deleted policy gets no requests.
- **Result**: the cluster enforces the current set of policies.

## Rules

| ID | Rule | Source | Held by |
|---|---|---|---|
| jsadmission.R1 | Only requests that match `rules`, selectors and `matchPolicy` reach the script. A change or a deletion takes effect at once. | `JSAdmissionSpec` | `TestRegistrar_Update_OverwritesEntry`, `TestRegistrar_RemoveLastEntry_DeletesConfig`, `TestRegistrar_MixedValidatingAndMutating`, `TestRegistrar_TwoValidating_OneVWC_TwoEntries`, `TestServer_UnknownPolicy_Returns404`, `TestE2E` |
| jsadmission.R2 | A validating policy calls `validate(req)`, a mutating one `mutate(req)`. A missing function is a failed call. | `jsadmission.Handle` | `TestHandle_Validate_Allow`, `TestHandle_MissingExport` |
| jsadmission.R3 | `req` has `uid`, `kind`, `resource`, `subResource`, `name`, `namespace`, `operation`, `userInfo`, `object`, `oldObject` and `dryRun`; `object` and `oldObject` are objects, not strings. | `jsadmission.AdmissionRequest` | `TestServer_PassesEveryRequestFieldToScript` |
| jsadmission.R4 | A result without `allowed` denies. | `jsadmission.AdmissionResult` | `TestServer_MissingAllowedDenies_NullOrUndefinedFails` |
| jsadmission.R5 | `message`, `code` and `warnings` reach the apiserver. | admission.k8s.io/v1 | `TestHandle_Validate_Deny` |
| jsadmission.R6 | A mutating policy's `modifiedObject` becomes a JSON patch of the difference; no difference, no patch. | `jsadmission.CreatePatch` | `TestCreatePatch_NoChange`, `TestServer_Mutate_AddsLabel_AsJSONPatch`, `TestE2E` |
| jsadmission.R7 | A validating policy's `modifiedObject` is ignored. | `jsadmission.AdmissionResult` | `TestServer_Validate_IgnoresModifiedObject` |
| jsadmission.R8 | A patch that cannot be computed admits the request with a warning. | `fillResponse`; reason open in ADM-8 | missing → ADM-8 |
| jsadmission.R9 | A failed call follows `failurePolicy`: `Ignore` admits with a warning, anything else denies with code 500. | `JSAdmissionSpec.FailurePolicy` | `TestServer_Validate_FailurePolicy_Fail_OnJSThrow`, `TestServer_Validate_FailurePolicy_Ignore_OnJSThrow` |
| jsadmission.R10 | `failurePolicy` means the same whether the operator is unreachable or the call fails, and gojsop never waits longer than the apiserver does. | `jsadmission.Registrar` | missing → GATE-17 |
| jsadmission.R11 | A call ends at the smaller of `spec.timeoutSeconds` (default 5 s) and `spec.limits.timeoutSeconds` (default 30 s) and then counts as failed. | `callTimeout` | `TestCallTimeout_SmallerOfWebhookAndLimits`; the failure path: missing → GATE-22 |
| jsadmission.R12 | A failed call (error, memory limit, timeout, crash, client gone) affects only its request. The next request starts from the prepared script; nothing is prepared again. | js-registry.R2 | missing → GATE-22 |
| jsadmission.R13 | A policy can only read the cluster: `kube.get` and `kube.list`, no `kube.apply` or `kube.delete`. | `sideEffects: None` | `TestSharedFactory_ForAdmission_ReadOnlySurface` |
| jsadmission.R14 | Every response carries the UID of its request. | admission.k8s.io/v1 | `TestServer_Validate_AllowedRoundtrip` |
| jsadmission.R15 | Requests in the operator's own namespace never reach a policy, nor do those in `--admission-exclude-namespaces` (default `kube-system`, `cert-manager`). | decision "exclude system namespaces" | `TestFlags_AdmissionExclude_DefaultsToSystemNamespaces`, `TestE2E` |
| jsadmission.R16 | A denial carries no patch. | admission.k8s.io/v1 | `TestServer_Mutate_Denied_HasNoPatch` |
| jsadmission.R17 | When gojsop cannot write the webhook configurations, the policy is `Ready=False`, reason `WebhookSyncFailed`. | status-conditions.R1 | `TestReconcile_RegistrarSyncFailure_ShowsReadyFalse` |
| jsadmission.R18 | While a new script is prepared the policy is `Ready=False`, reason `Building`. | status-conditions.R7 | `TestReconcile_BuildStates_ShowBuildingThenBuildFailed` |
| jsadmission.R19 | While no script is ready, `failurePolicy` decides every request at once; no request waits for the prepare. | js-registry.R19 | `TestServer_NoVM_AppliesFailurePolicyAtOnce` |
| jsadmission.R20 | Every operator replica answers every policy, also before and without leader election. | decision "replicated admission path" | `TestJSAdmissionServerReconciler_EveryReplicaAnswers`, `TestJSAdmissionServerReconciler_DeletionUnregisters`, `TestServerController_IsNotLeaderElected`; with `replicas: 2` missing → GATE-28 |
| jsadmission.R21 | A failed call records a Warning event `ReviewFailed` on the policy, with a fixed message that never carries the script's error text. | status-conditions.R3 | `TestServer_Emits_ReviewFailed_OnJSThrow` |
| jsadmission.R22 | A script that fails to prepare shows `BuildFailed` and is retried with backoff; a new source is tried at once. | status-conditions.R7, js-registry.R17 | `TestReconcile_BuildStates_ShowBuildingThenBuildFailed` |
| jsadmission.R23 | A result of `undefined` or `null` is a failed call. | `jsadmission.Handle` | `TestServer_MissingAllowedDenies_NullOrUndefinedFails` |
| jsadmission.R24 | Server-managed fields and `/status` are never in the patch. | admission.k8s.io/v1 | `TestCreatePatch_FiltersImmutable` |
| jsadmission.R25 | gojsop retries writing the webhook configurations until it succeeds. | `jsadmission.Registrar` | `TestRegistrar_SyncFailure_IsRetriedAndReported` |
| jsadmission.R26 | A renewed webhook certificate reaches the webhook configurations within a minute, without a policy change. | decision "TLS with cert-manager" | `TestRegistrar_CAChange_RewritesConfigsWithoutPolicyChange` |

## Aspects

- [js-execution](../aspects/js-execution.md): deadline, memory limit and error classes of a call.
- [js-registry](../aspects/js-registry.md): how a script is prepared and called.
- [js-sources](../aspects/js-sources.md): where the source comes from.
- [kube-access](../aspects/kube-access.md): the `kube` object, read-only here.
- [status-conditions](../aspects/status-conditions.md): how the policy reports its state.

## Decisions

- **One `ValidatingWebhookConfiguration` (`gojsop-validating`) and one `MutatingWebhookConfiguration` (`gojsop-mutating`) hold all policies.** Status: accepted (2026-05). Why: one object to own and clean up. Not taken: one configuration per policy; reason not recorded.
- **`jsadmission.Registrar` is the only writer of both and deletes one when it is empty.** Status: accepted (2026-05). Why: one owner of the aggregation.
- **Each policy is served at `/admission/validate/<ns|cluster>/<name>` or `/admission/mutate/...`, built only by `jsadmission.PathFor`.** Status: accepted (2026-05). Why: registrar and server must agree on one format.
- **A policy is served before it is published to the registrar.** Status: accepted (2026-05). Why: requests arrive as soon as the configuration points at the operator; an unknown path answers 404.
- **Webhook entries are named `<ns>-<name>.policies.gojsop.io`, cluster-scoped `<name>.policies.gojsop.io`.** Status: accepted (2026-05). Why: unique inside one configuration.
- **TLS with a cert-manager certificate; the registrar reads the CA bundle on every sync and polls it every minute.** Status: accepted (2026-05; polling 2026-10, ADM-4). Why: the apiserver rejects controller-runtime's self-signed certificate, and cert-manager renews the certificate without telling anyone (R26). Not taken: cert-manager's CA injector, because it only patches configurations it was told about by annotation, and the registrar rewrites them.
- **Admission scripts are prepared only by `SharedFactory.ForAdmission`, with a read-only `kube`.** Status: accepted (2026-05). Why: admission runs with `sideEffects: None`.
- **`kube-system` and `cert-manager` are excluded from every policy by default; `--admission-exclude-namespaces` changes the list, the operator's namespace is always out.** Status: accepted (2026-10, ADM-6). Why: under `failurePolicy: Fail` a broken policy or a down operator would otherwise block the cluster's own components and the issuer of the webhook certificate (R15). Not taken: no default exclusion, because one careless rule on `pods` could then stop `kube-system`.
- **The admission path is replicated, the hook path is not.** Status: accepted (2026-10, ADM-12). Why: the Service routes requests to every ready pod; a replica that knows no policy answers 404, and under `failurePolicy: Fail` that denies requests cluster-wide. Hooks stay leader-only because `handle()` has side effects and must run once. Cost: every replica prepares every policy. Gain: the webhook answers from process start. Not taken: routing through the leader, which needs a second Service and an endpoint rewrite on every failover.
- **The scaffolded Kubebuilder webhook for the `JSAdmission` CRD validates nothing.** Status: proposed. Open: ADM-7.

## Open

ADM-3, ADM-7, ADM-8, EXEC-2, EXEC-4, STAT-4, OPS-1, GATE-7, GATE-13, GATE-17, GATE-18, GATE-22, GATE-28
