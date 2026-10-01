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

This block describes the Kubernetes API that gojsop exposes and the rules for changing it.

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

## Rules

- **R1** Change the API only in `api/v1alpha1/*.go`, run `make generate manifests`
  and commit the regenerated files in the same change. Never edit
  `zz_generated.deepcopy.go` or `config/crd/bases` by hand.
  Why: the next generation overwrites hand edits.
  Gate: `make test` regenerates but does not detect a stale commit; diff check
  missing → GATE-14.
- **R2** Mark every field `+required` or `+optional`. Required fields have no
  `omitempty`, optional fields have it.
  Why: the generated schema then states what the user must set.
  Gate: missing → GATE-15.
- **R3** Express closed value sets with `+kubebuilder:validation:Enum`,
  defaults with `+kubebuilder:default`, and cross-field constraints with
  `XValidation` and a `message`. Declare `+listType` on every slice. Enum values
  come from the Go constants that implement them.
  Why: the API server rejects bad objects before the controller sees them.
  Gate: envtest rejection cases missing → GATE-15. Violated today → API-5
  (restart reasons are a doc list, not an enum).
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

## Open

Tracked in [backlog](../backlog.md): API-1 to API-8; gates GATE-10, GATE-11,
GATE-14, GATE-15.
