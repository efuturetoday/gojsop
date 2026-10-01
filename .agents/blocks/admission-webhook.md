---
id: admission-webhook
status: accepted
entrypoints:
  - jsadmission.Server.ValidateHandler
  - jsadmission.Server.MutateHandler
  - jsadmission.Registrar.Sync
  - controller.JSAdmissionReconciler.Reconcile
  - jsadmission.Handle
---

# Admission Webhook

This block describes how gojsop turns a `JSAdmission` policy into a Kubernetes admission webhook.

A `JSAdmission` is cluster-scoped. It does not get its own webhook configuration. All validating policies become entries of one `ValidatingWebhookConfiguration` (`gojsop-validating`). All mutating policies become entries of one `MutatingWebhookConfiguration` (`gojsop-mutating`). A debounced `Registrar` writes both. Why one aggregated configuration won over one object per policy is not recorded.

The operator serves every policy from its own webhook server under `/admission/validate/<ns|cluster>/<name>` and `/admission/mutate/...`. The `Server` looks the policy up per request and runs the JavaScript function `validate(req)` or `mutate(req)` on the policy's persistent VM through `jsregistry.Registry.Call`. The [js-registry](js-registry.md) block covers VM lifecycle and rescue. The apiserver always reaches the operator over TLS with a cert-manager serving certificate.

Admission runs on the apiserver request path with `sideEffects: None`. Therefore policies get a read-only `kube` surface: `get` and `list`, no `apply` or `delete` (see [kube-access](kube-access.md)).

A script returns `{allowed, code?, message?, warnings?, modifiedObject?}`. A missing `allowed` means deny. A return of `undefined` or `null` is an error. For a mutating policy the operator turns `modifiedObject` into an RFC 6902 patch against the original object. Server-managed fields (uid, resourceVersion, generation, timestamps, managedFields, `/status`) are dropped from the patch. A validating policy that returns `modifiedObject` is logged and ignored. If the patch cannot be computed, the request stays allowed and gets a warning.

When the script fails, the `failurePolicy` decides. `Ignore` allows and adds a warning. Anything else denies with code 500. Panic, memory limit and the policy timeout also rescue the VM, so a stuck VM is rebuilt. A plain JavaScript error and a client disconnect do not rescue. The same `failurePolicy` and `timeoutSeconds` go into the webhook entry, so the apiserver applies them when the operator is unreachable. The default timeout is 5 s.

The webhook entry is named `<ns>-<name>.policies.gojsop.io` (cluster-scoped: `<name>.policies.gojsop.io`). `excludeNamespaces` adds a NotIn selector on the namespace name. Today it holds only the operator namespace.

The scaffolded Kubebuilder webhook for the `JSAdmission` CRD itself is an empty stub. It does not validate policies.

## Rules

- **R1** Build a policy URL only with `jsadmission.PathFor` and parse it only with `jsadmission.keyFromPath`.
  Why: the registrar and the server must agree on one path format.
  Gate: `TestPathFor_RoundTrip`.
- **R2** Register a policy in the `Server` before publishing it to the `Registrar`.
  Why: once the configuration points at the operator, requests arrive, and a missing server entry returns 404.
  Gate: missing → GATE-17 (controller tests).
- **R3** Feed `FailurePolicy` and `TimeoutSeconds` identically into `PolicyEntry` and `PolicyMeta`.
  Why: the apiserver and the handler must apply the same values.
  Gate: missing → GATE-17 (controller tests).
- **R4** Build admission VMs only with `SharedFactory.ForAdmission`.
  Why: a policy decides on a request and must not change the cluster.
  Gate: `TestSharedFactory_ForAdmission_ReadOnlySurface`. Wiring check missing → GATE-7.
- **R5** Write `gojsop-validating` and `gojsop-mutating` only from `jsadmission.Registrar`.
  Why: the registrar owns the aggregation and deletes a configuration when its policy set is empty.
  Gate: `TestRegistrar_*`.
- **R6** Run `validate` and `mutate` only through `jsregistry.Registry.Call`.
  Why: one place for the call lock, panic recovery and result classification (see [js-execution](js-execution.md) R2).
  Gate: missing → GATE-22.
- **R7** Deny when the script omits `allowed`, and treat a `null` or `undefined` return as an error.
  Why: a policy that forgets to decide must not allow by accident.
  Gate: `TestHandle_*`.
- **R8** Drop server-managed fields and `/status` from the patch, and let a patch error keep the request allowed with a warning.
  Why: the apiserver rejects or overwrites those fields. The reason for allowing on a diff error is not recorded.
  Gate: `TestCreatePatch_*`, `TestServer_Mutate_AddsLabel_AsJSONPatch`.
- **R9** Apply `failurePolicy` on every script failure: `Ignore` allows with a warning, anything else denies with code 500.
  Why: the policy author chooses between availability and enforcement.
  Gate: `TestServer_Validate_FailurePolicy_Fail_OnJSThrow`, `TestServer_Validate_FailurePolicy_Ignore_OnJSThrow`.
- **R10** Rescue the VM on panic, memory limit and own timeout. Do not rescue on a client disconnect or a plain JavaScript error.
  Why: a cancelled wasm call leaves a broken VM only in the first three cases.
  Gate: missing → GATE-22.
- **R11** Echo the request UID in every response.
  Why: the apiserver matches the response to the request by UID.
  Gate: `TestServer_Validate_AllowedRoundtrip`.
- **R12** Serve the webhook with a certificate the apiserver trusts, and read the CA bundle on every sync.
  Why: without `--webhook-cert-path` controller-runtime self-signs, and the apiserver rejects that certificate.
  Gate: missing → GATE-23.

## Rejected

- One webhook configuration per policy. The reason is not recorded.
- Binding `kube.apply` and `kube.delete` for admission. Rejected because admission runs under `sideEffects: None`.
- Leaving the goroutine running after a timeout. Replaced when wazero gained context cancellation, because a stuck VM could not be rescued safely.

## Open

Tracked in [backlog](../backlog.md): ADM-1 to ADM-8, EXEC-1, EXEC-2, EXEC-4, STAT-4, OPS-1; gates GATE-7, GATE-13, GATE-17, GATE-18, GATE-22, GATE-23.
