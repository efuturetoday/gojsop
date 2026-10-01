# Block: JS Sources

Status: accepted

## Decision

JS source text is resolved by the controller, not by the registry. Each reconcile
calls one shared `jssource.Chain` with `spec.source`, hashes the bytes (sha256),
and passes `SourceHash` to `Registry.GetOrLoad`. A different hash rebuilds the VM;
the same hash reuses it. ConfigMap changes reach the reconcile through a watch plus
a mapper, not through polling.

Why (from `git log -- internal/jssource`):
- `aba0589` "add ConfigMap source loader with watch-driven reconcile": `configMapRef`
  was typed in the CRD but only the inline loader was wired, so users got "no loader
  matched". Watch instead of polling so edits trigger a reconcile.
- `64426b4` carved the package out of the controllers. Reason for the `Loader` /
  `Chain` shape (first loader that claims the source wins): not recorded.
- Rejected alternatives (mounted volumes, poll interval, per-controller loaders):
  not recorded.

## Code

- API: `JSSource` (`api/v1alpha1/js_shared.go:30`) with `Inline`, `ConfigMapRef`
  (`ConfigMapKeyRef`, `:51`), `OCI` (`OCISource`, `:71`). The CEL rule "exactly one of"
  is at `:29`. Embedded by `JSHookSpec.Source` (`jshook_types.go:27`) and
  `JSAdmissionSpec.Source` (`jsadmission_types.go:59`). `ConfigMapRef.Namespace` is
  required because both CRs are cluster-scoped; `Key` defaults to `hook.js` (`:61`).
- Entry point: `Chain.Load` (`internal/jssource/loader.go:28`). `Loader.Load` returns
  `(body, claimed, err)` (`loader.go:16`). The first loader with `claimed=true` wins;
  an error from any loader aborts. No claim gives "no loader matched source" (`:38`).
- Loaders: `InlineLoader` (`loader_inline.go:12`, claims when `Inline != ""`);
  `ConfigMapLoader` (`loader_configmap.go:35`, claims when `ConfigMapRef != nil`).
  Errors from a claimed ref: missing name or namespace, CM not found, key missing,
  value empty (Data first, then BinaryData; `:43-74`). Chain order matters only
  for fall-through, the CRD allows exactly one field.
- OCI: no loader exists. `spec.source.oci` is accepted by the CRD and fails with
  "no loader matched" (SRC-1). The `Chain` doc at `loader.go:21` ("MVP wires only
  inline") is stale.
- Hashing: `Hash` (`loader.go:43`), sha256 hex of the body only. Callers:
  `jshook/controller/controller.go:158`, `jsadmission/controller/controller.go:137`.
  The hash goes into `BuildOptions.SourceHash` (`controller.go:174`, `:154`) and into
  `status.instance.sourceHash` (`controller.go:254`, `:259`; `js_shared.go:141`).
- Rebuild: `Registry.GetOrLoad` compares `existing.Opts.SourceHash` with the new hash
  twice, before and after the build lock (`internal/jsregistry/registry.go:233,246`).
  Only the hash is compared; no other `BuildOptions` field triggers a rebuild there.
- Watch: both `SetupWithManager` add `Watches(&corev1.ConfigMap{}, EnqueueRequestsFromMapFunc(...))`
  (`jshook/controller/controller.go:345-349`, `jsadmission/controller/controller.go:329-333`).
  Mappers `JSHookConfigMapMapper` / `JSAdmissionConfigMapMapper`
  (`internal/jssource/watch.go:26,34`) list all CRs on every ConfigMap event and match
  `ConfigMapRef` by namespace and name (`:73`). No field index, no predicate. A `List`
  error drops the event silently (`:49-54`).
- Reads: `ConfigMapLoader.Reader` is the manager's cache-backed client, so the watch
  starts a cluster-wide ConfigMap informer. RBAC `get;list;watch` on `configmaps`
  (`jshook/controller/controller.go:100`, `jsadmission/controller/controller.go:87`,
  `config/rbac/role.yaml:10`).
- Wiring: `cmd/main.go:252-255` builds one `Chain{InlineLoader, ConfigMapLoader}` and
  passes it to both reconcilers (`:259`, `:288`). A nil `Loader` falls back to inline-only
  (`jshook/controller/controller.go:338`, `jsadmission/controller/controller.go:322`).
- Errors: a load error goes to `fail` / `failAdmission` with event reason
  `SourceLoadFailed` (`conditions.EventSourceLoadFailed`, `internal/conditions/conditions.go:38`;
  call sites `jshook/controller/controller.go:154`, `jsadmission/controller/controller.go:133`).
  That sets `Ready=False` reason `Failed`, `status.lastReconcile.error = "source: <err>"`,
  and requeues after 5 s (`jshook/controller/controller.go:277-297`). The event message is the
  fixed string "source loader failed" (the error text lives only in the condition and log).
  The registry is not touched on this path, so an existing VM stays in place and keeps
  serving the previous source (inferred from the code, not tested).

## Rules

- Do: add a new source kind as a `Loader` in `internal/jssource`, register it in the
  chain in `cmd/main.go`, and add its CRD field to the `JSSource` CEL rule.
- Do: return `claimed=false` only when the field is unset; return `claimed=true` with an
  error for a configured but broken ref, so `SourceLoadFailed` fires.
- Do: keep error strings out of event messages (recorder dedupes on reason plus message).
- Don't: load sources inside `jsregistry`; the registry only sees bytes and a hash.
- Don't: put secrets in `spec.source.inline` (readable in etcd and by anyone with `get` on the CR; SRC-2).
- Don't: ship a CRD source field without a loader (SRC-1, API-1).

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| Chain: inline match, no source errors, hash stable | `go test ./internal/jssource -run 'TestChain_InlineMatches\|TestChain_NoSourceErrors\|TestHash_Stable'` | yes (`make test`, `.github/workflows/test.yml`) |
| ConfigMap loader: default key, explicit key, missing CM / key, empty value, no ref, binary data, order after inline | `-run 'TestConfigMapLoader_\|TestChain_ConfigMapAfterInline'` | yes |
| Watch mapper fan-out | `-run 'TestJSHookConfigMapMapper_\|TestJSAdmissionConfigMapMapper_'` | yes |
| Source change rebuilds VM | `go test ./internal/jsregistry -run TestRegistry_RestartOnSourceChange` | yes |
| End to end: ConfigMap edit triggers rebuild | none: `test/integration/jshook_controller_test.go` only uses a ConfigMap as a binding target (grep) | missing. Add an envtest: create CM, JSHook with `configMapRef`, update the CM, expect `status.instance.sourceHash` change. |
| `SourceLoadFailed` event and condition | none: no test references `EventSourceLoadFailed` or `SourceLoadFailed` (grep) | missing. Add an envtest with a dangling `configMapRef`, expect `Ready=False`, the event, and a recovery once the CM exists. |
| OCI source rejected or loaded | none | missing. Add a test that `oci` yields `SourceLoadFailed` until SRC-1 is done. |
| Mapper for JSAdmission with CRs in the same list type | `TestJSAdmissionConfigMapMapper_FansOutMatchingPolicies` covers match only; no no-match case for admission | missing. Low priority. |
| Lint | `make lint` (`.github/workflows/lint.yml`) | yes |

Gates marked `missing` are commitments, not facts.

## Open

Tracked in [backlog](../backlog.md): SRC-1 to SRC-6, REG-7, API-1, API-2, STAT-2, OPS-3; gates GATE-9, GATE-16.
