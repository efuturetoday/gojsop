---
id: js-sources
status: accepted
entrypoints:
  - jssource.Chain.Load
  - jssource.Hash
  - jssource.ConfigMapLoader.Load
  - jsregistry.Registry.GetOrLoad
---

# JS Sources

This block describes how gojsop turns `spec.source` of a JSHook or JSAdmission into the JavaScript text that a VM runs.

The controller resolves the source, not the registry. Each reconcile calls one shared `jssource.Chain` with `spec.source`. The chain asks its loaders in order, and the first loader that claims the source returns the bytes. The controller hashes the bytes (sha256) and passes the hash as `SourceHash` to `jsregistry.Registry.GetOrLoad`. A new hash rebuilds the VM. The same hash reuses it. The registry sees only bytes and a hash. See [js-registry](js-registry.md) for the rebuild itself.

Two loaders exist: inline and ConfigMap. `spec.source.oci` is accepted by the CRD, but no loader serves it, so it fails with "no loader matched" (SRC-1). A `configMapRef` needs an explicit namespace, because both CRs are cluster-scoped. The key defaults to `hook.js`.

ConfigMap edits reach the reconcile through a watch plus a mapper, not through polling. The mapper lists all CRs on every ConfigMap event and matches by namespace and name. The loader reads through the manager's cache, so the watch starts a cluster-wide ConfigMap informer.

A load error sets `Ready=False`, writes the event `SourceLoadFailed` and requeues. The registry is not touched on this path. An existing VM probably keeps serving the previous source. This is inferred from the code and untested (SRC-5).

The history records one reason: ConfigMap support was added with a watch because `configMapRef` was typed in the CRD but no loader served it. The reason for the `Loader` and `Chain` shape, and for the alternatives to the watch, is not recorded.

## Rules

- **R1** Resolve source text in the controller through `jssource.Chain.Load`. Never load sources inside `jsregistry`.
  Why: the registry only sees bytes and a hash, so it stays independent of where source lives.
  Gate: missing → GATE-2.
- **R2** A loader returns `claimed=false` only when its field is unset. A configured but broken ref returns `claimed=true` with an error.
  Why: only then does the controller report `SourceLoadFailed` instead of "no loader matched".
  Gate: `TestConfigMapLoader_NoRefFallsThrough`, `TestConfigMapLoader_MissingCM`, `TestConfigMapLoader_MissingKey`, `TestConfigMapLoader_EmptyValue`.
- **R3** Trigger a rebuild only through `jssource.Hash` of the source bytes.
  Why: the hash is the one restart trigger the registry compares.
  Gate: `TestHash_Stable`, `TestRegistry_RestartOnSourceChange`.
- **R4** Reach the reconcile from ConfigMap changes through a watch and a mapper.
  Why: edits must apply without a poll interval. Why not polling is not recorded.
  Gate: `TestJSHookConfigMapMapper_FansOutMatchingHooks`, `TestJSAdmissionConfigMapMapper_FansOutMatchingPolicies`. End to end missing → GATE-16.
- **R5** Keep error text out of the `SourceLoadFailed` event message.
  Why: the recorder dedupes on reason plus message.
  Gate: missing → GATE-16. See SRC-6 for the cost of this rule.
- **R6** Add a CRD source field only together with its loader.
  Why: a typed field without a loader fails at runtime with "no loader matched".
  Gate: missing → GATE-10. Violated today → SRC-1.
- **R7** Do not put secrets in `spec.source.inline`.
  Why: the CR is readable in etcd and by anyone with `get` on it.
  Gate: review only — a secret in a string cannot be detected reliably. Violated today → SRC-2 (no CRD warning).

## Rejected

Mounted volumes, a poll interval and per-controller loaders were considered. Their reasons are not recorded.

## Open

Tracked in [backlog](../backlog.md): SRC-1 to SRC-6, REG-7, API-1, API-2, STAT-2, OPS-3; gates GATE-2, GATE-9, GATE-10, GATE-16.
