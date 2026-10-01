# Block: CRD API Surface

Status: accepted

## Decision

The API is two cluster-scoped CRDs, `JSHook` and `JSAdmission`, group `core`,
domain `gojsop.io`, version `v1alpha1` (`PROJECT`, `api/v1alpha1/groupversion_info.go`).
Types in `api/v1alpha1` are the source of truth; the CRD YAML is generated.
Generic Kubebuilder rules come from the skill at
https://www.skills.sh/configbutler/skills/kubebuilder-api-design. The page
only summarises scope (type definitions, schema markers, status/conditions,
scaffold and regenerate); the page has no link to a raw SKILL.md, so the rules
below are standard Kubebuilder practice, not a verbatim copy of that skill.

- Spec holds user intent, status holds observation. Status rules live in
  [status-conditions](status-conditions.md); do not repeat them here.
- A JSHook spec has only `source` and `limits` (`jshook_types.go:24-32`).
  Bindings (kubernetes, schedule, onStartup, jqFilter, queue, allowFailure) are
  not CRD fields; they are returned by the script's `config()`. The API-1 rule
  therefore covers `config()` keys and status as well as CRD fields.
- While on v1alpha1, breaking changes are allowed. Policy: still edit in place,
  note the break in the commit message. No conversion webhook, no second
  version exists. Not decided by the maintainers in writing; this is the
  working assumption.
- Rejected alternatives: not recorded.

## Code

- Types: `api/v1alpha1/jshook_types.go`, `jsadmission_types.go`,
  shared `js_shared.go` (`JSSource` :30, `OCISource` :71, `JSLimits` :92).
- Generated, never hand-edited: `api/v1alpha1/zz_generated.deepcopy.go`,
  `config/crd/bases/core.gojsop.io_jshooks.yaml`, `..._jsadmissions.yaml`.
- Regeneration: `make generate` (deepcopy, `Makefile:48`) and `make manifests`
  (CRDs, RBAC, webhook config, `Makefile:44`). `make test`, `build`, `run`,
  `test-e2e`, `deploy` depend on both (`Makefile:61,89,112,116`).

Generic rule against project, checked in the source:

| Rule | Project | Evidence |
|------|---------|----------|
| Spec and status split, `subresource:status` | follows | `jshook_types.go:66`, `jsadmission_types.go:155` |
| Root types carry `object:root`, list types exist | follows | `jshook_types.go:65,96`, `jsadmission_types.go:154,186` |
| Required vs optional explicit (`+required`/`+optional`, no `omitempty` on required) | follows | `jsadmission_types.go:58-59,76-77`; required fields have no `omitempty` |
| `omitempty` on optional fields, `omitzero` on struct `metadata`/`status` | follows | `jshook_types.go:85,93` |
| Enums and defaults via markers, default only on optional fields | follows | `jsadmission_types.go:68-69,81-82,87-88,112-113`, `js_shared.go:61,94,103` |
| Numeric bounds on numbers | follows | `js_shared.go:94-96,103-105`, `jsadmission_types.go:104-106` |
| Bounds on strings and lists (`MaxLength`, `MaxItems`) | violated | no `MaxLength`/`MaxItems` anywhere; `inline` (`js_shared.go:34-35`) is unbounded, etcd object limit is ~1.5 MiB (unsure of server config) |
| CEL for cross-field rules | follows | exactly-one-of source at `js_shared.go:29`, tag or digest at `:70` |
| List semantics declared (`listType`) | follows | `jsadmission_types.go:28-44,148-149`, `jshook_types.go:44,59-60`, `js_shared.go:153` |
| Conditions are `[]metav1.Condition` map keyed by `type` | follows | `jshook_types.go:59-62` |
| Print columns for `kubectl get` | follows | `jshook_types.go:68-72`, `jsadmission_types.go:157-162` |
| Short names, scope marker matches `PROJECT` | violated | markers say `scope=Cluster` (`jshook_types.go:67`, `jsadmission_types.go:156`), `PROJECT` says `namespaced: true` for both kinds |
| Enum for closed string sets in status | violated | `JSRestartEvent.Reason` is a doc list only (`js_shared.go:118-121`), see API-5 |
| Field docs that say what is implemented | partial | `Source` docs do not say `oci` fails (API-2, SRC-1) |
| API-1: no field without implementation or inactive status | partial | `status.bindings` lists unfired schedule/onStartup (STAT-5, DISP-1); `source.oci` accepted but not loadable (SRC-1) |

## Rules

- Do: change the API only in `api/v1alpha1/*.go`, then run `make generate manifests`
  and commit the regenerated files in the same change.
- Do: mark every field `+required` or `+optional`; give required fields no
  `omitempty`, optional ones `omitempty`.
- Do: express closed value sets with `+kubebuilder:validation:Enum` and defaults
  with `+kubebuilder:default`; express cross-field constraints with
  `XValidation` and a `message`.
- Do: declare `+listType` (`set`, `map` with `listMapKey`, or `atomic`) on every slice.
- Do: add `MaxLength` / `MaxItems` to new strings and lists (commitment, not yet
  followed by existing fields).
- Do: write field comments that state what happens today, since they become
  `kubectl explain` text.
- Do: keep status in `Status` only; new status fields go through
  [status-conditions](status-conditions.md).
- Do: update `samples/` and the JS `config()` examples when a field changes.
- Don't: edit `zz_generated.deepcopy.go` or anything in `config/crd/bases` by
  hand; the next generation overwrites it.
- Don't: add a CRD field, `config()` key or status entry that has no
  implementation and no status or doc marking it inactive (API-1).
- Don't: add a second API version or a conversion webhook without a decision
  recorded in the backlog.
- Don't: use `+kubebuilder:validation:Enum` values that differ from Go constants
  that implement them; reference one source (API-5).

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| Generated files compile and tests run on fresh generation | `make test` runs `manifests generate fmt vet` (`Makefile:61`) in `.github/workflows/test.yml` | yes, but a stale committed CRD is regenerated silently and not detected |
| Generated files committed and current | proposed: after `make manifests generate`, CI runs `git diff --exit-code api config` | missing |
| Envtest loads CRDs from `config/crd/bases` | `make test` integration suites (`test/integration`) | yes |
| CEL and enum rules reject bad CRs | proposed: envtest cases creating invalid JSHook/JSAdmission (two sources, tag and digest, bad enum) | missing |
| Every spec field and `config()` key has an owner | proposed: table test (GATE-10) | missing |
| `RestartReason` constants match CRD enum | proposed: GATE-11 | missing |
| Scope in markers equals `PROJECT` | proposed: test comparing `PROJECT` with CRD `spec.scope` | missing |
| Samples validate against the CRD | proposed: envtest applies every file in `samples/` | missing |

No workflow contains a `diff` step (grep of `.github/workflows/*.yml`).

Gates marked `missing` are commitments, not facts.

## Open

Tracked in [backlog](../backlog.md): API-1 to API-8; gates GATE-10, GATE-11, GATE-14, GATE-15.
