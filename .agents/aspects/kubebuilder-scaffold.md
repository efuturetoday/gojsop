---
id: kubebuilder-scaffold
status: accepted
entrypoints:
  - main.main
---

# Kubebuilder Scaffold

This aspect describes how gojsop is laid out as a Kubebuilder project and who
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
`test/e2e/`. The [testing](testing.md) aspect covers them. The distribution
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

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once? | The Kubebuilder CLI (`PROJECT`, `kubebuilder create api`, `kubebuilder create webhook`) scaffolds; controller-gen (`make manifests generate`, version `CONTROLLER_TOOLS_VERSION` in the `Makefile`) generates CRDs, RBAC, webhook manifests and deepcopy code. Searched `Makefile`, `PROJECT`, `config/`, `api/`. |
| example | Which real use should others copy? | The `JSHook` kind: `api/v1alpha1/jshook_types.go`, controller in `internal/jshook/controller/`, sample in `config/samples/`. |
| test helper | How does a test use the aspect without effort? | envtest loads the generated `config/crd/bases` in `test/integration` (suite `TestControllers`); no other helper. Searched `test/`, `Makefile`. |
| sides | Which sides does it touch? | Back end only (Go types, controllers, webhook) plus infrastructure (CRD, RBAC and webhook YAML in `config/`). |
| tie | How do the sides stay in step? | Generated: `make manifests generate` produces the YAML and deepcopy code from the Go types and markers. No check that the output is committed yet, GATE-14. |

## How to use it

Adding a kind:

1. Run `kubebuilder create api --group core --version v1alpha1 --kind <Kind>`
   (add `kubebuilder create webhook` when it needs one). R3, R7.
2. Move the scaffolded controller from `internal/controller/` to the domain
   package, like `internal/jshook/controller/`. R3.
3. Edit the `*_types.go` file and its markers by hand, keep the
   `// +kubebuilder:scaffold:*` comments. R5, R6.
4. Run `make manifests generate` and commit the generated files. R1.
5. Add a sample under `config/samples/`.
6. Run `make lint-fix` and `make test`. R2.

## Rules

- **R1** After editing `*_types.go`, `+kubebuilder:` markers or RBAC markers, run
  `make manifests generate` and commit the generated files in the same change.
  Why: the CRDs, RBAC and deepcopy code must match the Go types.
  Gate: missing → GATE-14.
- **R2** After editing Go code, run `make lint-fix` and `make test`.
  Why: `make test` regenerates, formats and vets before it runs the tests.
  Gate: `make test` (runs in CI).
- **R3** Scaffold new kinds and webhooks with `kubebuilder create api` or
  `kubebuilder create webhook`, then move the controller next to its domain
  package like the existing ones.
  Why: the layout keeps controllers next to their domain code.
  Gate: review only — the layout deviates from the scaffold and no tool knows where a controller belongs.
- **R4** Run e2e tests only against an isolated Kind cluster (`make test-e2e`,
  `hack/e2e.sh`), never against a real cluster.
  Why: the tests create and delete cluster resources.
  Gate: `make test-e2e`.
- **R5** Never edit generated files: `zz_generated.*.go`, `config/crd/bases`,
  `config/rbac/role.yaml`, `config/webhook/manifests.yaml`, `PROJECT`
  (change it only through the Kubebuilder CLI). `deploy/chart/templates/`
  is not in Git: `make chart` writes it from `config/`.
  Why: the next regeneration overwrites the edit.
  Gate: missing → GATE-14.
- **R6** Never delete `// +kubebuilder:scaffold:*` comments.
  Why: the Kubebuilder CLI injects new code at these markers.
  Gate: review only — a deleted marker only shows at the next scaffold run.
- **R7** Never use `kubebuilder ... --force` without a backup of custom logic.
  Why: `--force` overwrites scaffolded files.
  Gate: review only — the Kubebuilder CLI does not report an overwrite.

## Decisions

- **Controllers live next to their domain code, not in `internal/controller/`.**
  Status: accepted (reason not recorded).
  Why: not recorded. Not taken: the Kubebuilder default layout.
- **Distribute a Helm chart and an `install.yaml`, both built from `config/`; release with release-please.**
  Status: accepted (2026-10, OPS-4). Why: Helm is what most clusters install
  with, `install.yaml` serves the rest, and generating both from the one
  Kustomize tree keeps them equal. The chart comes from Kubebuilder's
  `helm/v2-alpha` plugin: `make chart` writes `deploy/chart/templates` from
  `config/` in CI and in the release job, and only `Chart.yaml` and
  `values.yaml` are in Git, so the chart cannot drift from `config/`
  (2026-10-05, replaces committed templates in `dist/chart`). release-please
  turns the conventional commits into versions and bumps the image tag in
  `config/manager`, `Chart.yaml` and `values.yaml`. The image, the chart
  (`oci://ghcr.io/efuturetoday/charts/gojsop`) and `install.yaml` are
  published by `.github/workflows/release.yml`.
  Not taken: a hand-written chart, because it drifts from `config/`.
- **A namespaced Role is written by hand (`config/rbac/manager_namespace_role.yaml`), not by an RBAC marker.**
  Status: accepted (2026-10). Why: controller-gen names a generated Role
  like the ClusterRole (`manager-role`), and the Helm plugin keys files on
  that name, so one overwrote the other and the chart lost the operator's
  right to impersonate the ServiceAccounts of hooks.
  Reported upstream as kubernetes-sigs/kubebuilder#5546. PR #5579 (v4.14)
  added a switch that renders all rules as either a Role or a ClusterRole;
  with v4.16.0 a Role and a ClusterRole of the same name are still merged
  (checked 2026-10-05), so the hand-written Role stays.

## Open

Tracked in [backlog](../backlog.md): GATE-14, OPS-4.
