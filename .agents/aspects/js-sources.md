---
id: js-sources
status: accepted
entrypoints:
  - jssource.Chain.Load
  - jssource.Hash
  - jssource.ConfigMapLoader.Load
  - jsregistry.Registry.Ensure
---

# JS Sources

This aspect describes how gojsop turns `spec.source` of a JSHook or JSAdmission into the JavaScript text it prepares.

The controller resolves the source, not the registry. Each reconcile calls one shared `jssource.Chain` with `spec.source`. The chain asks its loaders in order, and the first loader that claims the source returns the bytes. The controller hashes the bytes (sha256) and passes the hash as `SourceHash` to `jsregistry.Registry.Ensure`. A new hash prepares the script again; the same hash keeps it. The registry sees only bytes and a hash. See [js-registry](js-registry.md) for the rebuild itself.

Two loaders exist: inline and ConfigMap. `spec.source.oci` is accepted by the CRD, but no loader serves it, so it fails with "no loader matched" (SRC-1). A `configMapRef` needs an explicit namespace, because both CRs are cluster-scoped. The key defaults to `hook.js`.

ConfigMap edits reach the reconcile through a watch plus a mapper, not through polling. The mapper lists all CRs on every ConfigMap event and matches by namespace and name. The loader reads through the manager's cache, so the watch starts a cluster-wide ConfigMap informer.

A load error sets `Ready=False`, writes the event `SourceLoadFailed` and requeues. The registry is not touched, so a script that was ready keeps serving the previous source; no test shows it (SRC-5).

The history records one reason: ConfigMap support was added with a watch because `configMapRef` was typed in the CRD but no loader served it. The reason for the `Loader` and `Chain` shape, and for the alternatives to the watch, is not recorded.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once, so nobody builds it a second time? | `jssource.Chain` with the loaders `jssource.InlineLoader` and `jssource.ConfigMapLoader`, built once in `cmd/main.go` and shared by both controllers. `jssource.Hash` makes the restart trigger. Searched `internal/` and `cmd/` for other loads of `spec.source`: none. |
| example | Which real use in the code should others copy? | `jssource.ConfigMapLoader.Load` (the loader) and `jssource.JSHookConfigMapMapper` (the watch). |
| test helper | How does a test use the aspect without effort? | `newReader` in the `jssource` tests builds a controller-runtime fake client with ConfigMaps for `jssource.ConfigMapLoader`; `jssource.NewChain(jssource.InlineLoader{})` is the loader of the controller integration tests. |
| sides | Which sides does it touch? | Back end only: both controllers (`jshook`, `jsadmission`) and the registry. |
| tie | How do the sides stay in step? | n/a, because one side only. Searched for a second consumer of `spec.source` outside the controllers: none. The CRD field against loader gap is rule R6 (GATE-10). |

## How to use it

Add a source kind (for example OCI, SRC-1):

1. Add the field to `JSSource` in the CRD types and regenerate the manifests.
2. Write a loader with `Load(ctx, src) ([]byte, bool, error)`: `claimed=false` only when its field is unset (R2).
3. Register it in the `jssource.NewChain` call in `cmd/main.go`.
4. If the source can change outside the CR, add a watch and a mapper like `jssource.JSHookConfigMapMapper` (R4).
5. Add a test for each failure path, with the rule ID in a comment.

## Rules

- **R1** Resolve source text in the controller through `jssource.Chain.Load`. Never load sources inside `jsregistry`.
  Why: the registry only sees bytes and a hash, so it stays independent of where source lives.
  Gate: `TestImportBoundary_EngineStaysBehindRegistry`, `TestChain_InlineMatches`, `TestChain_ConfigMapAfterInline`, `TestChain_NoSourceErrors`.
- **R2** A loader returns `claimed=false` only when its field is unset. A configured but broken ref returns `claimed=true` with an error.
  Why: only then does the controller report `SourceLoadFailed` instead of "no loader matched".
  Gate: `TestConfigMapLoader_NoRefFallsThrough`, `TestConfigMapLoader_MissingCM`, `TestConfigMapLoader_MissingKey`, `TestConfigMapLoader_EmptyValue`.
- **R3** Trigger a rebuild only through `jssource.Hash` of the source bytes.
  Why: the hash is the one restart trigger the registry compares.
  Gate: `TestHash_Stable`, `TestRegistry_RestartOnSourceChange`.
- **R4** Reach the reconcile from ConfigMap changes through a watch and a mapper.
  Why: edits must apply without a poll interval. Why not polling is not recorded.
  Gate: `TestJSHookConfigMapMapper_FansOutMatchingHooks`, `TestJSHookConfigMapMapper_NoMatch`, `TestJSAdmissionConfigMapMapper_FansOutMatchingPolicies`. End to end missing → GATE-16.
- **R5** Keep error text out of the `SourceLoadFailed` event message.
  Why: the recorder dedupes on reason plus message.
  Gate: missing → GATE-16
  See SRC-6 for the cost of this rule.
- **R6** Add a CRD source field only together with its loader.
  Why: a typed field without a loader fails at runtime with "no loader matched".
  Gate: missing → GATE-10. Violated today → SRC-1.
- **R7** Do not put secrets in `spec.source.inline`.
  Why: the CR is readable in etcd and by anyone with `get` on it.
  Gate: review only — a secret in a string cannot be detected reliably. Violated today → SRC-2 (no CRD warning).
- **R8** A `configMapRef` reads its `key`, `hook.js` when unset, from `data`, and from `binaryData` when `data` lacks it.
  Why: authors keep scripts in ConfigMaps as text or as binary; both must work without extra fields.
  Gate: `TestConfigMapLoader_DefaultKey`, `TestConfigMapLoader_ExplicitKey`, `TestConfigMapLoader_BinaryData`.

## Decisions

- **One shared `jssource.Chain` serves both controllers.** Status: accepted. Why: one place loads `spec.source` (R1).
- **ConfigMap edits arrive through a watch, not a poll interval; mounted volumes are not used.** Status: accepted. Reasons not recorded.

## Open

Tracked in [backlog](../backlog.md): SRC-1 to SRC-6, API-1, API-2, STAT-2, OPS-3; gates GATE-10, GATE-16.
