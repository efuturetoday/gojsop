# Block: Kubebuilder Scaffold

Status: accepted

## Decision

gojsop is a Kubebuilder v4 project (`go.kubebuilder.io/v4`, CLI 4.11.1,
`PROJECT`). Kubebuilder owns the scaffold, the generated code and the
manifests. Project code owns everything under `internal/`.

- One API group `core.gojsop.io`, one version `v1alpha1`, two namespaced kinds:
  `JSHook` (controller) and `JSAdmission` (controller plus defaulting and
  validation webhooks) (`PROJECT`). Single-group layout, `api/v1alpha1/`.
- Deviation from the default layout: controllers do not live in
  `internal/controller/`. They live next to their domain code:
  `internal/jshook/controller/`, `internal/jsadmission/controller/`, and the
  webhook in `internal/jsadmission/webhook/v1alpha1/`. Reason not recorded;
  the effect is that `kubebuilder create` output must be moved by hand.
- Integration tests live in `test/integration/` (envtest), e2e tests in
  `test/e2e/` (Kind). See [testing](testing.md).
- Distribution path (Kustomize bundle or Helm chart) is not chosen yet (OPS-4).

## Code

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

Scaffold markers the CLI injects at: `cmd/main.go:52,65,305`,
`test/integration/suite_test.go:37,66`, `test/e2e/e2e_test.go:226,318`,
`internal/jsadmission/webhook/v1alpha1/webhook_suite_test.go:43,72,115`.

## Rules

- Do: after editing `*_types.go`, markers or RBAC markers run
  `make manifests generate` and commit the generated files in the same change.
- Do: after editing Go code run `make lint-fix` and `make test`.
- Do: scaffold new kinds and webhooks with `kubebuilder create api` /
  `kubebuilder create webhook`, then move controllers next to their domain
  package as the existing ones are.
- Do: run e2e tests only against an isolated Kind cluster (`make test-e2e`,
  `hack/e2e.sh`), never against a real cluster.
- Don't: edit generated files (`zz_generated.*.go`, `config/crd/bases`,
  `config/rbac/role.yaml`, `config/webhook/manifests.yaml`, `PROJECT`).
- Don't: delete `// +kubebuilder:scaffold:*` comments.
- Don't: use `kubebuilder ... --force` without a backup of custom logic.

Generic references: [Kubebuilder Book](https://book.kubebuilder.io),
[Good Practices](https://book.kubebuilder.io/reference/good-practices.html),
[Markers](https://book.kubebuilder.io/reference/markers.html),
[API Conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md),
[controller-runtime FAQ](https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md).

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| Generated code builds and is formatted | `make test` runs `manifests generate fmt vet` first (`Makefile:61`) | yes (`test.yml`) |
| Generated files are committed | `make manifests generate && git diff --exit-code` | missing (GATE-14) |
| Scaffold markers present | grep for the markers listed above | missing, low value |

## Open

Tracked in [backlog](../backlog.md): GATE-14, OPS-4.
