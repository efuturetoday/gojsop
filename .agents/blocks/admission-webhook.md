# Block: Admission Webhook

Status: accepted

## Decision

A `JSAdmission` (cluster-scoped, `api/v1alpha1/jsadmission_types.go:156`) becomes one
entry in one of two central webhook configurations (`gojsop-validating`,
`gojsop-mutating`). The operator serves all policies from its own webhook server
under `/admission/validate/<ns|cluster>/<name>` and `/admission/mutate/...`; the
HTTP `Server` looks the policy up per request and runs `validate(req)` or
`mutate(req)` on that policy's persistent VM through `Registry.Call` (see
[js-registry](js-registry.md), not repeated here). The apiserver always talks to
the operator over TLS with a cert-manager issued serving cert.

Why (from `git log -- internal/jsadmission`):
- `5d60be4` "JSAdmission CRD with central VWC/MWC and JS dispatcher": one aggregated
  VWC and MWC, written by a debounced registrar, instead of one configuration object
  per policy. Reason beyond the commit title: not recorded.
- `2667c98` "bind read-only kube.* surface": admission runs on the apiserver request
  path under `sideEffects: None`, so `kube.apply` / `kube.delete` are not bound
  (comment `controller.go:60-62`).
- `fcb159e` "rescue VM on panic and OOM in review" and `61d1fe4` (ctx-aware VM): a
  timeout now cancels the wasm call (`server.go:217-223`), so a stuck VM is rebuilt
  instead of leaking a goroutine.
- Rejected alternatives: not recorded in git.

## Code

- CRD rules: `Type` validating|mutating default validating (`jsadmission_types.go:68-71`);
  `Rules` MinItems=1 (`:76`), each rule needs apiGroups, apiVersions, resources, operations
  (all MinItems=1, ops enum `CREATE;UPDATE;DELETE;CONNECT;*`, `:26-53`); `FailurePolicy`
  Fail|Ignore default Fail (`:81-84`); `MatchPolicy` default Equivalent (`:87-90`);
  `TimeoutSeconds` 1..30 default 5 (`:104-108`); `SideEffects` default None (`:112-115`);
  `NamespaceSelector`, `ObjectSelector` passed through (`:95,99`). Status:
  `webhookPath`, `webhookConfigName`, `instance`, `lastReconcile`, conditions (`:119-152`).
- Controller: `JSAdmissionReconciler.Reconcile` (`internal/jsadmission/controller/controller.go:108`).
  Order: load source (`:130`), mint read-only binder `KubeHost.ForAdmission` (`:143`),
  `Registry.GetOrLoad` with `admissionPostBuild` requiring the `validate` or `mutate`
  export (`:94-106,152-158`), manual restart annotation (`:172-192`), `Server.Register`
  first (`:207`, comment `:197-198`: a missing entry would 404 once the config exists),
  then `Registrar.Upsert` (`:227`), then status (`:241-266`). NotFound unregisters from
  server and registrar and drops the VM (`:113-123`).
- Registrar: `Registrar.Upsert/Remove` mark dirty and schedule `Sync` after `Debounce`
  (default 200 ms, `registrar.go:112-130`). `Sync` reads the CA, splits by type and
  rewrites both configs (`:134-170`). An empty set deletes the config (`:173-175,197-199`).
  Webhook name `<ns>-<name>.policies.gojsop.io` (`DNSWebhookName`, `:282`); cluster-scoped
  policies get only `<name>.policies.gojsop.io`. `admissionReviewVersions` is `["v1"]`.
  `mergeNSSelector` appends a NotIn on `kubernetes.io/metadata.name` for
  `ExcludeNamespaces` (`:230-245`); `cmd/main.go:282` excludes only the operator's own namespace.
- Server: `Server.ValidateHandler/MutateHandler` (`server.go:147,154`) mounted on the
  manager's webhook server (`cmd/main.go:274-275`). `serve` (`:160`) rejects non-POST (405),
  bad path or unknown policy or type mismatch (404), bad body or missing `request` (400),
  body cap 3 MiB (`:417`). Single path builder `PathFor` (`:117`).
- `review` (`server.go:224`) always echoes the UID. Policy not loaded or VM vanished
  mid-call goes to `applyFailurePolicy` (`:228-232,257-262`).
- JS contract (`handle.go:90`): `validate(req)` / `mutate(req)` is a global function. The
  request has `uid, kind, resource, subResource, name, namespace, operation, userInfo,
  object, oldObject, dryRun` (`handle.go:41-53`); object fields are real JS objects. Return
  `{allowed, code?, message?, warnings?, modifiedObject?}` (`:57-71`). Omitted `allowed`
  means deny (`:58-60`). `undefined` or `null` return is an error (`:113-115`).
  `modifiedObject` is used only for mutating policies; a validating policy returning it is
  logged and ignored (`server.go:331-335`).
- Patch: `fillResponse` (`server.go:323`) calls `CreatePatch(req.Object.Raw, modifiedObject)`
  (`diff.go:33`), an RFC 6902 diff via jsonpatch. Ops under `/metadata/uid`,
  `resourceVersion`, `generation`, `creationTimestamp`, `deletionTimestamp`,
  `deletionGracePeriodSeconds`, `selfLink`, `managedFields` and `/status` are dropped
  (`diff.go:18-28,64-71`). Empty result means no patch. A diff error does not reject:
  allowed stays and a `gojsop: failed to compute patch` warning is added (`server.go:340-347`).
- Failure policy (`applyFailurePolicy`, `server.go:364`): `Ignore` allows and adds a
  `gojsop: <reason>` warning; anything else denies with code 500 and
  `gojsop admission error: <reason>`. The same value is also written to the webhook entry
  (`registrar.go:184,208`), so the apiserver applies it too when the operator is unreachable.
- Timeout and rescue: `review` wraps the request context in `WithTimeout(entry.Timeout)`,
  default 5 s (`server.go:241-246`). Outcomes (`:264-318`): panic, memory limit and our own
  deadline call `jslifecycle.Rescue` (reasons `ReasonPanic`, `ReasonMemoryLimit`,
  `ReasonTimeout`) and then the failure policy; a client disconnect does not rescue
  (`:285-294`); a plain JS error does not rescue and emits `EventReviewFailed` (`:305-313`).
  The same `timeoutSeconds` goes to the webhook entry, so apiserver and handler share one value.
- TLS: manager serves certs from `--webhook-cert-path` (`cmd/main.go:165-174`); without it
  controller-runtime self-signs, so the apiserver would not trust it (development only).
  `fileCABundleProvider` (`cmd/main.go:86`) reads `ca.crt`, falls back to `tls.crt`, on every
  `Sync`. Deployment: `config/certmanager/` (self-signed `Issuer`, `Certificate serving-cert`,
  secret `webhook-server-cert`), mount patch `config/default/manager_webhook_patch.yaml`,
  DNS names filled by kustomize replacements (`config/default/kustomization.yaml:120-160`).
  `make install-certmanager` applies cert-manager `$(CERT_MANAGER_VERSION)` and waits for its
  three deployments (`Makefile:180-183`); the e2e suite installs it unless
  `CERT_MANAGER_INSTALL_SKIP=true` (`test/e2e/e2e_suite_test.go:72`).
- Kube access: `SharedFactory.ForAdmission` returns `ReadOnlyKubeHost` (`internal/jsengine/kubehost/factory.go:53`),
  which binds only `kube.get` and `kube.list` (`kubehost.go:65-80`).
- Scaffold: `internal/jsadmission/webhook/v1alpha1/jsadmission_webhook.go` is the Kubebuilder
  defaulter/validator for the `JSAdmission` CRD itself, both empty `TODO(user)` stubs
  (`:55-60,77-100`). It does not validate policies. Registered unless `ENABLE_WEBHOOKS=false` (`cmd/main.go:299`).

## Rules

- Do: build a policy's URL only with `PathFor` and parse only with `keyFromPath`.
- Do: register in `Server` before publishing to `Registrar` (a config entry without a server entry 404s).
- Do: keep `FailurePolicy` and `TimeoutSeconds` identical in `PolicyEntry` and `PolicyMeta`
  (both fed from the spec at `controller.go:207-238`).
- Do: build admission VMs with `KubeHost.ForAdmission`; policies must stay read-only.
- Don't: write `gojsop-validating` / `gojsop-mutating` from anywhere but `Registrar`.
- Don't: run `validate` / `mutate` outside `Registry.Call`.
- Don't: ship without cert-manager (or another source of `webhook-cert-path` plus `ca.crt`).

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| Allow roundtrip, 404 unknown policy, path roundtrip | `go test ./internal/jsadmission -run 'TestServer_Validate_AllowedRoundtrip\|TestServer_UnknownPolicy_Returns404\|TestPathFor_RoundTrip'` | yes (`make test`) |
| Failure policy Fail and Ignore on JS throw, `ReviewFailed` event | `-run 'TestServer_Validate_FailurePolicy_Fail_OnJSThrow\|TestServer_Validate_FailurePolicy_Ignore_OnJSThrow\|TestServer_Emits_ReviewFailed_OnJSThrow'` | yes |
| Mutation becomes JSONPatch | `-run TestServer_Mutate_AddsLabel_AsJSONPatch` | yes |
| `Handle` allow, deny, mutate, missing export, throw | `-run TestHandle_` | yes |
| Patch diff, no-op, immutable filtered | `-run TestCreatePatch_` | yes |
| Registrar: aggregation, mixed types, delete on empty, overwrite, webhook name | `-run 'TestRegistrar_\|TestDNSWebhookName_Cluster'` | yes |
| Admission binder is read-only | `go test ./internal/jsengine/kubehost -run TestSharedFactory_ForAdmission_ReadOnlySurface` | yes |
| Controller reconciles a JSAdmission | `test/integration/jsadmission_controller_test.go` ("should successfully reconcile the resource") via `make test` | yes, but checks no status (GATE-13) |
| Timeout path: deadline hit, rescue, failurePolicy applied | none: no server test uses an infinite loop or a deadline | missing. Add `TestServer_Review_Timeout_RescuesAndAppliesFailurePolicy` in `internal/jsadmission/server_test.go` (policy `for(;;){}`, `Timeout` 100 ms, assert deny, `EventReviewTimeout`, `RestartsByReason[timeout]==1`). |
| Panic and memory-limit paths | none | missing. Same file, two tests next to the above. |
| Failure policy on invalid input (nil `request`, wrong method, body over 3 MiB) | none | missing. Add `TestServer_BadRequests` with 400/405 cases. |
| `ExcludeNamespaces` merged into selectors | none (`mergeNSSelector` untested) | missing. Add `TestRegistrar_MergeNSSelector` in `registrar_test.go`. |
| Real apiserver calls a policy over TLS | none: e2e (`test/e2e/e2e_test.go`) has no JSAdmission case | missing. Add an e2e case: apply a validating sample, `kubectl apply` a violating object, expect denial. |
| Admission VM has no `kube.apply` / `kube.delete` | only the factory type test above | missing (GATE-7). |

Gates marked `missing` are commitments, not facts.

## Open

Tracked in [backlog](../backlog.md): ADM-1 to ADM-7, EXEC-1, EXEC-2, EXEC-4, STAT-4, OPS-1; gates GATE-7, GATE-13, GATE-18.
