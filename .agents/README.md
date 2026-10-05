# Project knowledge for agents and people

gojsop follows the agentic SDLC library
([efuturetoday/agentic-sdlc](https://github.com/efuturetoday/agentic-sdlc)).
The method is in the skill `sdlc-core` (`.agents/skills/sdlc-core/SKILL.md`):
what an area and an aspect are, rule IDs, gates and open items.

## Areas

What gojsop promises the engineers who write JavaScript for it.

| Area | Covers |
|---|---|
| [jshook](areas/jshook.md) | JSHook: `spec.bindings`, Synchronization and events, `handle()` |
| [jsadmission](areas/jsadmission.md) | JSAdmission: which requests reach the script, its answer, failure policy |
| [workspace](areas/workspace.md) | The `gojsop` CLI: run and test hooks and policies without a cluster |

## Aspects

Ways of doing things that both areas share.

| Aspect | Covers |
|---|---|
| [js-execution](aspects/js-execution.md) | QuickJS on wazero, the `jsrun.Runner` port, limits, cancellation, host APIs |
| [js-registry](aspects/js-registry.md) | One VM per resource behind `jsrun.Runner`, build and restart |
| [js-sources](aspects/js-sources.md) | `spec.source`, loaders, source hash |
| [kube-access](aspects/kube-access.md) | `kube.*` from JavaScript, RBAC |
| [status-conditions](aspects/status-conditions.md) | CRD status, `Ready`, restart bookkeeping |
| [api-design](aspects/api-design.md) | CRD markers, validation, versioning |
| [kubebuilder-scaffold](aspects/kubebuilder-scaffold.md) | Generated files, scaffold markers, layout |
| [testing](aspects/testing.md) | Test layers, Make targets, CI |
| [npm-packages](aspects/npm-packages.md) | The packages in `sdk/`: TypeScript, Biome, versions, publishing |

Open items: [backlog.md](backlog.md). Check: `make sdlc-check`.
