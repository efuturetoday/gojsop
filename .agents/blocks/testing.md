---
id: testing
status: accepted
entrypoints: []
---

# Testing

This block describes how gojsop is tested: which layer holds which test, which
commands run them, and which gates run in CI.

There are three layers, each with its own cost. Unit tests are the default.
They sit next to the code under `internal/`, use the standard `testing`
package and are named like `TestRegistry_RestartByKey_RebuildsFromCachedSource`.
envtest integration tests (Ginkgo) in `test/integration` run the controllers
against a real API server without a kubelet. e2e tests in `test/e2e` run the
built image on an isolated Kind cluster. They carry the build tag `e2e`, so a
plain `go test ./...` skips them.

`make test` runs unit and envtest tests together and writes `cover.out`, which
is gitignored. It does not use `-race`. `make test-e2e` creates the Kind
cluster, runs the e2e tests and removes the cluster. `make lint` runs
golangci-lint; the version is the same in the Makefile and in CI. CI runs
`make test`, `make lint` and `make test-e2e` on every push and pull request.
`make check-blocks` verifies that the blocks in `.agents/blocks` match the code.

One Ginkgo suite lives inside `internal`, in `internal/jsadmission/webhook/v1alpha1`.
It is a Kubebuilder scaffold. `hack/e2e.sh` is not part of the tests. It is a
demo (`make demo-e2e`) that syncs ConfigMaps on its own Kind cluster.

Mocking the API server in unit tests was rejected: envtest exists for that.
Per-package coverage numbers are in the backlog, see GATE-12.

## Rules

- **R1** Put tests for pure logic next to the code as `*_test.go`, with the
  standard `testing` package and table tests where inputs vary.
  Why: unit tests are the cheapest layer and the default.
  Gate: `make test`.
- **R2** Name unit tests `Test<Type>_<Behaviour>`.
  Why: a failing name then says what broke.
  Gate: review only.
- **R3** Put tests that need an API server (reconcile, status, CRD validation)
  in `test/integration` with Ginkgo and envtest.
  Why: envtest gives a real API server without a kubelet.
  Gate: `make test`.
- **R4** Put tests that need a real cluster, the image or cert-manager in
  `test/e2e` behind the `e2e` build tag.
  Why: they are slow, and plain `go test ./...` must stay fast.
  Gate: `make test-e2e`.
- **R5** Keep `ginkgolinter` clean. Use Ginkgo only for envtest and e2e.
  Why: unit tests stay plain `testing`.
  Gate: `make lint`.
- **R6** Make unit tests hermetic: use `httptest` recorders and in-memory
  state, no outbound network, no `KUBEBUILDER_ASSETS`.
  Why: unit tests must run anywhere without setup.
  Gate: review only.
- **R7** Do not add a Ginkgo suite under `internal` for new work.
  Why: the existing one is a Kubebuilder scaffold, not a pattern to copy.
  Gate: missing → GATE-18.
- **R8** Do not run `hack/e2e.sh` as a test, and do not commit `cover.out`.
  Why: the script is a demo, and `cover.out` is a build artefact.
  Gate: `.gitignore` lists `cover.out`; the e2e part is review only.
- **R9** Land every new gate from the backlog as a test plus a command that CI
  runs.
  Why: a gate that CI does not run is only a promise.
  Gate: missing → GATE-1.
- **R10** Keep blocks consistent with the code.
  Why: a block that contradicts the code misleads readers.
  Gate: `make check-blocks`.

## Open

Tracked in [backlog](../backlog.md): GATE-1 to GATE-20, among them GATE-12
(coverage floor) and GATE-20 (e2e cluster cleanup on failure).
