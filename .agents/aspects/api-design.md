---
id: api-design
status: accepted
entrypoints:
  - v1alpha1.JSHookSpec
  - v1alpha1.JSAdmissionSpec
  - v1alpha1.JSSource
  - v1alpha1.JSLimits
---

# CRD API Surface

This aspect describes the Kubernetes API that gojsop exposes and the rules for changing it.

The API is two cluster-scoped CRDs, `JSHook` and `JSAdmission`, in group `core`,
domain `gojsop.io`, version `v1alpha1`. The Go types in `api/v1alpha1` are the
source of truth. The CRD YAML, the deepcopy code and the RBAC and webhook
manifests are generated from them with `make generate` and `make manifests`.
Generic Kubebuilder practice applies; see [kubebuilder-scaffold](kubebuilder-scaffold.md).

Spec holds user intent, status holds observation. Status rules live in
[status-conditions](status-conditions.md).

`v1alpha1.JSHookSpec` has only `source` and `limits`. The bindings of a hook
(kubernetes, schedule, onStartup, jqFilter, queue, allowFailure) are not CRD
fields. The script returns them from `config()`. The no-dead-surface rule (R6)
therefore covers `config()` keys and status entries as well as CRD fields.

While the API is `v1alpha1`, breaking changes are allowed. The working policy
is to edit in place and note the break in the commit message. There is no
second version and no conversion webhook. The maintainers have not decided this
in writing (API-8).

The project does not yet bound strings and lists. No field has `MaxLength` or
`MaxItems`, so `spec.source.inline` is limited only by the etcd object size
(API-7). R4 applies to new fields only.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once? | controller-gen (`CONTROLLER_GEN` in the Makefile, targets `manifests` and `generate`) reads the `+kubebuilder:` markers of `api/v1alpha1` and writes the CRD YAML, RBAC, webhook manifests and deepcopy. No second way found: no hand-written CRD YAML or schema code exists (searched `config/crd/bases`, `api/`, `internal/`). |
| example | Which real use should others copy? | `v1alpha1.JSLimits` (default, Minimum, Maximum) and `v1alpha1.JSSource` (XValidation with message, MinLength). |
| test helper | How does a test use the aspect without effort? | The envtest suite `integration.TestControllers` loads `config/crd/bases` into a real API server. It has no helper for rejection cases yet (GATE-15). |
| sides | Which sides does it touch? | Go types, generated CRD YAML, RBAC and webhook manifests, `config/samples`, `PROJECT`, the engine defaults in `jsengine.DefaultLimits`, JS `config()` examples. |
| tie | How do the sides stay in step? | Generated from the Go types by controller-gen; the generated-diff check is missing (GATE-14). Engine defaults are tied by `TestDefaultLimits_MatchKubebuilderTags`. Samples, `PROJECT` and the restart-reason doc list have no tie (API-4, API-6, API-5). |

## How to use it

Adding a CRD field:

1. Add the field to the type in `api/v1alpha1`, with `+required` or `+optional`, the validation markers (R2, R3, R4) and a field comment that states what happens today (R5). Add no comment that is not meant for the CRD description.
2. Implement it, or mark it inactive (R6).
3. Run `make manifests generate` and commit the regenerated files (R1).
4. Update `config/samples` and the JS `config()` examples (R8).
5. Run `make test`; envtest loads the regenerated CRDs.

## Rules

- **R1** Change the API only in `api/v1alpha1/*.go`, run `make generate manifests`
  and commit the regenerated files in the same change. Never edit
  `zz_generated.deepcopy.go` or `config/crd/bases` by hand.
  Why: the next generation overwrites hand edits.
  Gate: missing → GATE-14.
- **R2** Mark every field `+required` or `+optional`. Required fields have no
  `omitempty`, optional fields have it.
  Why: the generated schema then states what the user must set.
  Gate: missing → GATE-15.
- **R3** Express closed value sets with `+kubebuilder:validation:Enum`,
  defaults with `+kubebuilder:default`, and cross-field constraints with
  `XValidation` and a `message`. Declare `+listType` on every slice. Enum values
  come from the Go constants that implement them.
  Why: the API server rejects bad objects before the controller sees them.
  Gate: `TestDefaultLimits_MatchKubebuilderTags` holds the defaults; envtest
  rejection cases for enums and CEL are tracked in GATE-15. Violated today →
  API-5 (restart reasons are a doc list, not an enum).
- **R4** Add `MaxLength` and `MaxItems` to every new string and list.
  Why: not recorded; the backlog entry names the etcd object size limit.
  Gate: missing → GATE-15. Violated today → API-7.
- **R5** Write field comments that state what happens today.
  Why: they become `kubectl explain` text.
  Gate: missing → GATE-10. Violated today → API-2.
- **R6** Add no CRD field, `config()` key or status entry without an
  implementation, unless a doc comment or status marks it inactive.
  Why: users must not rely on fields that do nothing.
  Gate: missing → GATE-10. Violated today → API-1.
- **R7** Keep scope markers equal to `PROJECT`.
  Why: two sources for one fact drift apart.
  Gate: missing → GATE-14. Violated today → API-6.
- **R8** Update `config/samples` and the JS `config()` examples when a field
  changes.
  Why: samples are the first thing users copy.
  Gate: missing → GATE-15. Violated today → API-4.
- **R9** Add no second API version and no conversion webhook without a
  recorded decision.
  Why: not recorded.
  Gate: missing → GATE-14.

## Decisions

- **The Go types in `api/v1alpha1` are the source of truth; CRD YAML, deepcopy, RBAC and webhook manifests are generated.** Status: accepted.
  Why: one source cannot drift from itself (R1).
  Not taken: hand-written CRD YAML, because it drifts from the types.
- **A hook's bindings (kubernetes, schedule, onStartup, jqFilter, queue, allowFailure) come from the script's `config()`, not from CRD fields.** Status: accepted.
  Why: the script is the one place a hook author states them; `JSHookSpec` stays `source` and `limits`.
  Not taken: CRD fields for bindings, because they would duplicate what the script returns.
- **While the API is `v1alpha1`, edit in place, note the break in the commit message, no second version and no conversion webhook.** Status: proposed (API-8; not decided in writing).
  Why: no users are recorded that need a migration path.
  Not taken: a second version with a conversion webhook, because of the cost before `v1beta1` (R9).

## Open

API-1, API-2, API-4, API-5, API-6, API-7, API-8, GATE-10, GATE-11, GATE-14, GATE-15
