---
id: testing
status: accepted
entrypoints: []
---

# Testing

This aspect describes how gojsop is tested: which layer holds which test, which
commands run them, and which gates run in CI.

There are three layers, each with its own cost. Unit tests are the default.
They sit next to the code under `internal/`, use the standard `testing`
package and are named like `TestRegistry_RestartByKey_RebuildsFromCachedSource`.
envtest integration tests (Ginkgo) in `test/integration` run the controllers
against a real API server without a kubelet. e2e tests in `test/e2e` run the
built image on an isolated Kind cluster. They carry the build tag `e2e`, so a
plain `go test ./...` skips them.

`make test` runs unit and envtest tests together and writes `cover.out`, which
is gitignored. It runs with `-race`. `make test-e2e` creates the Kind
cluster, runs the e2e tests and removes the cluster. `make lint` runs
golangci-lint; the version is the same in the Makefile and in CI. CI runs
`make test`, `make lint` and `make test-e2e` on every push and pull request.
`make sdlc-check` verifies that `.agents` matches the code; it replaced the former `hack/checkblocks`.

One Ginkgo suite lives inside `internal`, in `internal/jsadmission/webhook/v1alpha1`.
It is a Kubebuilder scaffold. `hack/e2e.sh` is not part of the tests. It is a
demo (`make demo-e2e`) that syncs ConfigMaps on its own Kind cluster.

Per-package coverage numbers are in the backlog, see GATE-12.

## Parts

| Part | Question | Answer |
|---|---|---|
| block | What code does the work, once? | Standard `testing` for unit tests; Ginkgo with envtest (`TestControllers` in `test/integration`) for controller tests; Kind with `TestE2E` in `test/e2e` for e2e. Make targets `test`, `test-e2e`, `lint`. Searched `Makefile`, `test/`, `internal/`. |
| example | Which real use should others copy? | `TestRegistry_RestartByKey_RebuildsFromCachedSource` for unit tests; `test/integration/jshook_controller_test.go` for envtest. |
| test helper | How does a test use the aspect without effort? | The envtest suites (`TestControllers` in `test/integration`, `TestAPIs` in `internal/jsadmission/webhook/v1alpha1`), `test/utils` (`utils.Run`, `utils.InstallCertManager`, `utils.LoadImageToKindClusterWithName`) for e2e, and controller-runtime and dynamic fake clients in unit tests (`fake.NewClientBuilder`, `fake.NewSimpleDynamicClient`). |
| sides | Which sides does it touch? | Back end and infrastructure (Kind cluster, CI workflows `test.yml`, `test-e2e.yml`, `lint.yml`). |
| tie | How do the sides stay in step? | n/a, because tests only run Go code against the cluster; the CI workflows call the same make targets as a developer (searched `.github/workflows`, `Makefile`). |

## How to use it

Adding a test that holds a rule:

1. Pick the layer: pure logic is a unit test next to the code (R1), API
   server needed is an envtest test in `test/integration` (R3), a real
   cluster or image is an e2e test behind the `e2e` tag (R4).
2. Name a unit test `Test<Type>_<Behaviour>` (R2).
3. Put the rule ID in a comment above the test function or inside it, for
   example `// js-registry.R3`, one line per rule it holds. For a Ginkgo
   spec, put the comment inside the `It` body.
4. Name the test in the rule's Gate, in backticks, and run `make test`.
5. Run `make sdlc-check`.

## Rules

- **R1** Put tests for pure logic next to the code as `*_test.go`, with the
  standard `testing` package and table tests where inputs vary.
  Why: unit tests are the cheapest layer and the default.
  Gate: `make test`.
- **R2** Name unit tests `Test<Type>_<Behaviour>`.
  Why: a failing name then says what broke.
  Gate: review only — a name convention that no tool checks.
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
  Gate: review only — imports and `testing` use cannot be told from intent by a tool.
- **R7** Do not add a Ginkgo suite under `internal` for new work.
  Why: the existing one is a Kubebuilder scaffold, not a pattern to copy.
  Gate: missing → GATE-18.
- **R8** Do not run `hack/e2e.sh` as a test, and do not commit `cover.out`.
  Why: the script is a demo, and `cover.out` is a build artefact.
  Gate: review only — `.gitignore` lists `cover.out`, and the e2e script is not run by any gate.
- **R9** Land every new gate from the backlog as a test plus a command that CI
  runs.
  Why: a gate that CI does not run is only a promise.
  Gate: missing → GATE-1.
- **R10** (withdrawn) Keep blocks consistent with the code.
  Why: it existed only for `hack/checkblocks`; `make sdlc-check` now checks
  every aspect and area.
  Gate: `make sdlc-check`.

## Decisions

- **Mocking the API server in unit tests is rejected.** Status: accepted
  (date and name not recorded).
  Why: envtest gives a real API server. Not taken: API server mocks, because
  they drift from real behaviour.

## Open

Tracked in [backlog](../backlog.md): GATE-1 to GATE-20, among them GATE-12
(coverage floor) and GATE-20 (e2e cluster cleanup on failure).
