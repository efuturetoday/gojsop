# gojsop - AI Agent Guide

gojsop is a Kubernetes operator that runs JavaScript: `JSHook` reacts to
cluster events, `JSAdmission` validates and mutates admission requests. The JS
runs in QuickJS on wazero, embedded in the operator.

## Start here

gojsop follows the agentic SDLC library. Load the skill `sdlc-core` first; it
explains areas, aspects, rule IDs, gates and open items.

1. Read [.agents/README.md](.agents/README.md) and the areas and aspects you
   touch before you change code.
2. If a change breaks a rule or decision, update the area or aspect in the
   same change (skills `sdlc-area`, `sdlc-aspect`).
3. Open items live in [.agents/backlog.md](.agents/backlog.md) (skill
   `sdlc-tracker-file-backlog`). Name the key in the commit that closes one.
4. Go code and tests link to rules with comments such as `// js-execution.R4`
   (skill `sdlc-stack-go`).
5. `make sdlc-check` and `make test lint` must pass.
6. Write commit messages with the skill `caveman-commit`. Commits carry no
   AI co-author trailer, and the author email is
   `12057167+efuturetoday@users.noreply.github.com`.
7. The `golang-*` skills give general Go guidance. Where one contradicts an
   area or aspect of this repository, the area or aspect wins.

## Critical rules

- Never edit generated files: `**/zz_generated.*.go`, `config/crd/bases/`,
  `config/rbac/role.yaml`, `config/webhook/manifests.yaml`, `PROJECT`.
  Details: [kubebuilder-scaffold](.agents/aspects/kubebuilder-scaffold.md).
- Never delete `// +kubebuilder:scaffold:*` comments.
- Run user JavaScript only through the port `jsrun.Runner` (`Invoke`); callers
  never import `jsengine` or `jsregistry`
  ([js-registry](.agents/aspects/js-registry.md), js-execution.R10).
- Run e2e tests only against an isolated Kind cluster.

## After making changes

```bash
make manifests generate   # after *_types.go or marker changes
make lint-fix             # after Go changes
make test                 # unit and envtest integration tests
make engine-wasm          # after changes to internal/jsengine/glue/glue.c; commit the new engine.wasm
```
