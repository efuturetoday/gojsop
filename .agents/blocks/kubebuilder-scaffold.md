---
id: kubebuilder-scaffold
status: accepted
entrypoints:
  - main.main
---

# Kubebuilder Scaffold

This block describes how gojsop is laid out as a Kubebuilder project and who
owns which file.

gojsop is a Kubebuilder v4 project (`go.kubebuilder.io/v4`, CLI 4.11.1, see
`PROJECT`). Kubebuilder owns the scaffold, the generated code and the
manifests. Project code owns everything under `internal/`.

The API has one group `core.gojsop.io`, one version `v1alpha1` and two
cluster-scoped kinds, both in `api/v1alpha1/`
(`PROJECT` wrongly says namespaced, API-6). `JSHook` has a controller.
`JSAdmission` has a controller plus defaulting and validation webhooks.

The layout deviates from the Kubebuilder default. Controllers do not live in
`internal/controller/`. They live next to their domain code, in
`internal/jshook/controller/` and `internal/jsadmission/controller/`. The
webhook lives in `internal/jsadmission/webhook/v1alpha1/`. The reason is not
recorded. The effect is that the output of `kubebuilder create` must be moved
by hand.

Integration tests (envtest) live in `test/integration/`, e2e tests (Kind) in
`test/e2e/`. The [testing](testing.md) block covers them. The distribution
path (Kustomize bundle or Helm chart) is not chosen yet.

| Path | Owner | Regenerate with |
|------|-------|-----------------|
| `api/v1alpha1/*_types.go` | project | edit by hand, then `make manifests generate` |
| `api/v1alpha1/zz_generated.deepcopy.go` | generated | `make generate` |
| `config/crd/bases/*.yaml` | generated | `make manifests` |
| `config/rbac/role.yaml` | generated from `+kubebuilder:rbac` markers | `make manifests` |
| `config/webhook/manifests.yaml` | generated | `make manifests` |
| `config/samples/*.yaml` | project | edit by hand |
| `PROJECT` | Kubebuilder CLI | `kubebuilder ...` only |
| `cmd/main.go` | project plus scaffold markers | edit by hand |

Generic references: [Kubebuilder Book](https://book.kubebuilder.io),
[Good Practices](https://book.kubebuilder.io/reference/good-practices.html),
[Markers](https://book.kubebuilder.io/reference/markers.html),
[API Conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md),
[controller-runtime FAQ](https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md).

## Rules

- **R1** After editing `*_types.go`, `+kubebuilder:` markers or RBAC markers, run
  `make manifests generate` and commit the generated files in the same change.
  Why: the CRDs, RBAC and deepcopy code must match the Go types.
  Gate: missing → GATE-14.
- **R2** After editing Go code, run `make lint-fix` and `make test`.
  Why: `make test` regenerates, formats and vets before it runs the tests.
  Gate: `make test` (runs in CI, `test.yml`).
- **R3** Scaffold new kinds and webhooks with `kubebuilder create api` or
  `kubebuilder create webhook`, then move the controller next to its domain
  package like the existing ones.
  Why: the layout keeps controllers next to their domain code.
  Gate: review only.
- **R4** Run e2e tests only against an isolated Kind cluster (`make test-e2e`,
  `hack/e2e.sh`), never against a real cluster.
  Why: the tests create and delete cluster resources.
  Gate: `make test-e2e`.
- **R5** Never edit generated files: `zz_generated.*.go`, `config/crd/bases`,
  `config/rbac/role.yaml`, `config/webhook/manifests.yaml`, `PROJECT`.
  Why: the next regeneration overwrites the edit.
  Gate: missing → GATE-14.
- **R6** Never delete `// +kubebuilder:scaffold:*` comments.
  Why: the Kubebuilder CLI injects new code at these markers.
  Gate: review only — low value.
- **R7** Never use `kubebuilder ... --force` without a backup of custom logic.
  Why: `--force` overwrites scaffolded files.
  Gate: review only.

## Open

Tracked in [backlog](../backlog.md): GATE-14, OPS-4.
