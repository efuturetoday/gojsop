# Block: Testing

Status: accepted

## Decision

Three layers, each with its own cost. Unit tests (`*_test.go` next to the code
under `internal/`) are the default. envtest integration tests (Ginkgo) cover
controllers against a real API server without kubelet. e2e tests run the built
image on an isolated Kind cluster. `make test` runs unit and envtest together
and writes coverage. Rejected: mocking the API server in unit tests (envtest
exists for that).

Layers (state of 2026-10-01):

- Unit: 9 package dirs under `internal/` have `*_test.go` (19 files), standard
  `testing`, names like `TestRegistry_RestartByKey_RebuildsFromCachedSource`
  (`internal/jsregistry/*_test.go`). `internal/jsadmission/server_test.go:50`
  uses `httptest.NewRecorder`, no socket.
- envtest, Ginkgo: `test/integration/suite_test.go:57` (`BeforeSuite`, CRDs from
  `config/crd/bases`, `:70`), specs in `jshook_controller_test.go` and
  `jsadmission_controller_test.go`. Second envtest suite inside `internal`:
  `internal/jsadmission/webhook/v1alpha1/webhook_suite_test.go:76`, which also
  dials TLS to the webhook (`:127`, localhost envtest server).
- e2e: `test/e2e/e2e_suite_test.go`, `e2e_test.go`, build tag `e2e`
  (`//go:build e2e`), so plain `go test ./...` skips them.
  Cluster `gojsop-test-e2e` (`Makefile:68`, `KIND_CLUSTER`).
- `hack/e2e.sh` is not the test runner: it is the configmap-sync demo on kind
  cluster `gojsop-e2e` (`hack/e2e.sh:14`), started by `make demo-e2e`. Nothing
  asserts on it beyond waiting for the synced ConfigMaps.

Make targets (`Makefile`):

- `make test` (`:61`): `manifests generate fmt vet setup-envtest`, then
  `go test $(go list ./... | grep -v /e2e) -coverprofile cover.out` with
  `KUBEBUILDER_ASSETS` from `setup-envtest use $(ENVTEST_K8S_VERSION)`. No `-race`.
- `setup-envtest` (`:225`): downloads envtest binaries into `bin/`; versions
  derived from `go.mod` (`:205`, `:210`).
- `make lint` / `lint-fix` / `lint-config` (`:98-106`): golangci-lint
  `v2.7.2` (`:214`).
- `make test-e2e` (`:89`): `setup-test-e2e`, then `go test -tags=e2e ./test/e2e/`,
  then `cleanup-test-e2e` (cluster likely stays when tests fail; unsure,
  recipe has no `||`).

CI (`.github/workflows`), all three on `push` and `pull_request`, ubuntu-latest:

- `test.yml`: `go mod tidy`, `make test`.
- `lint.yml`: `golangci/golangci-lint-action@v8`, version `v2.7.2`.
- `test-e2e.yml`: installs latest kind, `go mod tidy`, `make test-e2e`.

golangci (`.golangci.yml`, `default: none`): copyloopvar, dupl, errcheck,
ginkgolinter, goconst, gocyclo, govet, ineffassign, lll, modernize, misspell,
nakedret, prealloc, revive (comment-spacings, import-shadowing), staticcheck,
unconvert, unparam, unused. Formatters gofmt, goimports. `dupl` and `lll` are
off for `internal/*`; `lll` off for `api/*`.

Coverage: `cover.out` is gitignored (`.gitignore:30`) and produced only by
`make test`. Unit run `go test ./internal/... -count=1 -cover`, 2026-10-01
(no envtest assets set): all packages pass.
conditions 100.0, jsadmission 66.2, jsadmission/webhook/v1alpha1 0.0,
jsengine 49.3, jsengine/kubehost 67.5, jshook 75.0, jslifecycle 36.4,
jsregistry 67.4, jssource 88.1. No test files, 0.0: `jsadmission/controller`,
`jshook/controller`, `jshook/dispatcher`. The 0.0 for the webhook package is
likely because Ginkgo specs need envtest binaries; unsure whether they ran or
skipped.

Helpers: `test/utils/utils.go` is for e2e (`Run`, `InstallCertManager`,
`UninstallCertManager`, `IsCertManagerCRDsInstalled`,
`LoadImageToKindClusterWithName`, `GetNonEmptyLines`, `GetProjectDir`,
`UncommentCode`). Unit tests have no shared helper package.

## Code

- `Makefile:61` (`test`), `:98` (`lint`), `:89` (`test-e2e`), `:225` (`setup-envtest`).
- `.golangci.yml`, `.github/workflows/{test,lint,test-e2e}.yml`.
- `test/integration/suite_test.go`, `test/e2e/`, `test/utils/utils.go`.
- Single entry for the full suite: `make test` (unit and envtest); e2e only via `make test-e2e`.

## Rules

- Do: put pure logic tests next to the code as `*_test.go` in the same package tree, standard `testing`, table tests where inputs vary.
- Do: name tests `Test<Type>_<Behaviour>` as in `internal/jsregistry`.
- Do: put anything needing an API server (reconcile, status, CRD validation) in envtest with Ginkgo, in `test/integration`.
- Do: put anything needing a real cluster, image or cert-manager in `test/e2e` behind the `e2e` build tag.
- Do: keep `ginkgolinter` clean; Ginkgo is used only for envtest and e2e.
- Do: make unit tests hermetic; use `httptest` recorders, in-memory state, no outbound network, no `KUBEBUILDER_ASSETS`.
- Don't: add a Ginkgo suite under `internal/` for new work; the one in `internal/jsadmission/webhook/v1alpha1` is a Kubebuilder scaffold (backlog does not rule on it).
- Don't: run `hack/e2e.sh` as a test; it is a demo.
- Don't: commit `cover.out`.
- Do: new gates from the backlog land as tests plus a CI-run command (GATE-1).

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| Unit and envtest | `make test` | yes (`test.yml`) |
| gofmt, go vet | part of `make test` prerequisites (`fmt`, `vet`) | yes (via `test.yml`) |
| golangci-lint | `make lint` (CI uses the action, same version) | yes (`lint.yml`) |
| e2e on Kind | `make test-e2e` | yes (`test-e2e.yml`) |
| Lint config valid | `make lint-config` | no |
| Single `make gates` | none | missing, GATE-1 |
| Import boundaries | none | missing, GATE-2, GATE-8 |
| Race detector | none | missing, GATE-3 |
| Registry.Call outcomes, deadline, isolation | none | missing, GATE-4, GATE-5, GATE-6 |
| Admission VM host API | none | missing, GATE-7 |
| Status and spec/CRD consistency | none | missing, GATE-9, GATE-10, GATE-11, GATE-13 |
| Coverage floor | none | missing, GATE-12 |

Gates marked `missing` are commitments, not facts.

## Open

Tracked in [backlog](../backlog.md): GATE-1 to GATE-20.
