# gojsop - AI Agent Guide

gojsop is a Kubernetes operator that runs JavaScript: `JSHook` reacts to
cluster events, `JSAdmission` validates and mutates admission requests. The JS
runs in QuickJS on wazero, embedded in the operator.

## Start here

Project knowledge lives in [.agents/](.agents/README.md), one block per
decision. Each block names the decision, the code that implements it, the
rules and the gates that enforce it.

1. Read [.agents/README.md](.agents/README.md) and the blocks for the area you
   touch before you change code.
2. Open items and their keys are in [.agents/backlog.md](.agents/backlog.md).
   Name the key in the commit message when you close one.
3. If a change breaks a block's decision, update the block in the same change.
4. Write and change blocks as described in
   [.agents/skills/write-block](.agents/skills/write-block/SKILL.md);
   `make check-blocks` must pass.
5. Write commit messages with the caveman-commit skill
   ([.agents/skills/caveman-commit](.agents/skills/caveman-commit/SKILL.md)):
   Conventional Commits, imperative subject, body only for the non-obvious
   why. Name the backlog key when a commit closes an item.

## Critical rules

- Never edit generated files: `**/zz_generated.*.go`, `config/crd/bases/`,
  `config/rbac/role.yaml`, `config/webhook/manifests.yaml`, `PROJECT`.
  Details: [kubebuilder-scaffold](.agents/blocks/kubebuilder-scaffold.md).
- Never delete `// +kubebuilder:scaffold:*` comments.
- Run user JavaScript only through `Registry.Call`
  ([js-registry](.agents/blocks/js-registry.md)).
- Run e2e tests only against an isolated Kind cluster.

## After making changes

```bash
make manifests generate   # after *_types.go or marker changes
make lint-fix             # after Go changes
make test                 # unit and envtest integration tests
```
