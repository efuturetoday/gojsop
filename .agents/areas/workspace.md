---
id: workspace
status: proposed
entrypoints:
  - workspace.Load
  - workspace.Manifest.Run
  - workspace.Serve
---

# Workspace

This area describes what a maintainer who writes hooks and policies can do
on their own machine: test them with vitest, and run them once with the
`gojsop` CLI, without a cluster and with the result the cluster would give.

The maintainer keeps hooks and policies in a repository of their own, one
directory each: the manifest (`hook.yaml` or `policy.yaml`, a `JSHook` or
`JSAdmission`), the script next to it (`policy.ts`, `hook.ts`, or plain
`.js`), and tests (`*.test.ts`). Tests are
vitest tests that use the npm package `@gojsop/testing`. That package
starts the `gojsop` binary and hands every call to it.

The script always runs in the engine the operator uses, with the same
`kube.*` surface, rights and limits; only the cluster is replaced by one
in memory. vitest and the test code run in Node; the script never does.

`npm create @gojsop` sets up such a repository with one example of each.
npm installs the `gojsop` binary for the platform with `@gojsop/testing`;
no Go is needed.

Words used here:

- **fake cluster**: objects held in memory for one call; `kube.*` and
  `event.all()` read and write them.
- **serve**: `gojsop serve --stdio`, one process per vitest worker that
  runs calls for `@gojsop/testing`.

## Use cases

### workspace.UC4 Start a workspace

- **Actor**: maintainer with Node, without Go
- **Trigger**: runs `npm create @gojsop my-policies`
- **Steps**:
  1. The command copies a workspace into `my-policies`: `package.json` with vitest and `@gojsop/testing`, one example policy and one example hook with tests (workspace.R10).
  2. `npm install` installs the `gojsop` binary for the platform (workspace.R9).
  3. `npm test` runs the example tests.
- **Exceptions**: a directory that exists and is not empty is left alone; the command fails.
- **Result**: the maintainer has green tests to copy from in a minute.

### workspace.UC1 Test a policy with vitest

- **Actor**: policy maintainer
- **Trigger**: runs `npx vitest`
- **Before**: `policy.yaml`, its script, a `*.test.ts` that imports `policy` from `@gojsop/testing`
- **Steps**:
  1. `policy("./policy.yaml").review(request, { cluster })` sends the call to `gojsop serve` (workspace.R4).
  2. The script runs in the engine against the fake cluster; the result comes back: `allowed`, `message`, `warnings`, the object after a mutation, console lines.
  3. The test asserts on it with `expect`.
- **Exceptions**: a script that throws, times out or hits its memory limit makes `review` reject with that error; serve keeps running (workspace.R5).
- **Result**: the maintainer knows what the policy decides, with the tools they already use.

### workspace.UC2 Test a hook with vitest

- **Actor**: hook maintainer
- **Trigger**: runs `npx vitest`
- **Before**: `hook.yaml`, its script, a test that builds a fake cluster with fixtures (`pod()`, `configMap()`, `namespace()`)
- **Steps**:
  1. `hook("./hook.yaml").handle(event, { cluster })` runs `handle(event)` in the engine; `kube.*` and `event.all()` work on the fake cluster.
  2. The test asserts on the fake cluster after the call.
- **Exceptions**: a `kube.*` call without the right in `spec.permissions` throws `Forbidden`, as in the cluster (workspace.R3).
- **Result**: the maintainer knows what the hook would change, before it runs anywhere.

### workspace.UC3 Run a script once

- **Actor**: maintainer
- **Trigger**: `gojsop run <manifest> --request <file>` or `--event <file>`, optionally `--cluster <file>` and `--trace`
- **Steps**: the CLI runs one call and prints the result as JSON; with `--trace` also every `kube.*` call with its arguments and answer.
- **Result**: a quick answer to "what would my policy say to this object?"

## Rules

| ID | Rule | Source | Held by |
|---|---|---|---|
| workspace.R1 | gojsop runs a script through the same engine, registry and `kube.*` binder as the operator; only the dynamic client is a fake. | decision "engine only" | `TestRun_UsesTheOperatorsEngineAndKubeSurface` |
| workspace.R2 | The script comes from `spec.source.inline`, or else from the one `.js` file next to the manifest. | decision "TypeScript through Vite" | `TestLoad_ScriptInlineOrBesideTheManifest` |
| workspace.R3 | `kube.*` has exactly the rights of `spec.permissions` (and, for a hook, read on its bindings); anything else throws `Forbidden`. | kube-access.R11 | `TestFakeCluster_EnforcesPermissions` |
| workspace.R4 | `gojsop serve --stdio` reads one JSON request per line (`review` or `handle`, manifest path, input, cluster) and writes one JSON answer per line: the result, console lines, the fake cluster after, or an error. | decision "vitest only" | `TestServe_AnswersReviewAndHandle` |
| workspace.R5 | A script that throws, times out or hits its memory limit yields an error answer naming which; the limits are the manifest's, and serve keeps answering. | js-execution.R3, js-execution.R5 | `TestServe_ErrorsAndLimitsKeepServing` |
| workspace.R6 | `event.all()` returns the fake cluster's objects of the event's binding, filtered by its selectors; the fake cluster after the call holds what the hook wrote. | jshook.R26 | `TestRun_HookChangesTheFakeCluster` |
| workspace.R7 | `@gojsop/testing` runs every `review` and `handle` through one `gojsop serve` per vitest worker and stops it when the worker ends. | decision "vitest only" | missing → WS-10 |
| workspace.R8 | `gojsop run --trace` prints every `kube.*` call with its arguments and its answer or error. | decision "no debugger" | `TestRun_TraceShowsKubeCalls` |
| workspace.R9 | Installing `@gojsop/testing` or `@gojsop/cli` brings the `gojsop` binary of the release with the same version for macOS and Linux (x64, arm64) and Windows (x64); `GOJSOP_BIN` overrides it. | decision "npm carries the binary" | missing → WS-10 |
| workspace.R11 | A caller may hand serve the script with the request; it replaces the `.js` next to the manifest, and together with `spec.source.inline` it is an error. `@gojsop/testing`, `gojsop build` and `npx gojsop run` hand over `policy.ts` or `hook.ts` (or the one module script) bundled with Vite, its exports as globals; an error in it names the TypeScript line. | decision "TypeScript through Vite" | `TestServe_SourceFromTheCaller` |
| workspace.R10 | `npm create @gojsop <dir>` creates a workspace whose tests pass after `npm install`, and refuses a directory that is not empty. | decision "npm carries the binary" | missing → WS-10 |

Every rule is held by a test or is `missing → <KEY>`.

## Aspects

- [js-execution](../aspects/js-execution.md): the engine, limits and the entry point contract gojsop shares.
- [js-registry](../aspects/js-registry.md): how gojsop prepares and calls a script, as the operator does.
- [kube-access](../aspects/kube-access.md): the `kube.*` surface and the rights the fake cluster enforces.
- [npm-packages](../aspects/npm-packages.md): how the npm packages are built, checked and published.

## Decisions

- **Scripts run only in the operator's engine, never in Node.** Status: accepted (2026-10-05). Why: Node differs in async, globals, language features, speed, limits and the JSON border, so a green Node run proves nothing; Cloudflare left its Node emulation (Miniflare) for its real runtime for the same reason. Not taken: a Node debug mode, because it would be a second truth.
- **Tests are vitest tests through `@gojsop/testing`; there is no second way.** Status: accepted (2026-10-05). Why: JS developers know vitest, its editor integration, watch mode, UI and reporters; one way keeps the docs, the examples and the code small. vitest needs Node, the script still runs in the engine through `gojsop serve`. Not taken: YAML cases and a built-in `gojsop test` runner with its own `gojsop:test` API, because two ways to test split users and maintenance.
- **TypeScript through Vite; the operator takes one JavaScript file.** Status: accepted (2026-10-05). Why: scripts are written as modules in TypeScript (`export function validate`), with imports and types from `@gojsop/types`. `@gojsop/testing` bundles them with the Vite that vitest brings before every call, and `gojsop build` writes the same bundle to `dist/` for the ConfigMap, so tests and cluster run the same code. The operator stays unchanged: one ES2023 script with global entry points (WS-5). A plain `.js` script still works as it is. Not taken: bundling in the operator, because the cluster would then build code nobody tested; esbuild as our own bundler, see the aspect npm-packages.
- **npm carries the binary; the release tag carries the version.** Status: accepted (2026-10-05). Why: JS developers install with npm, and an optional dependency per platform (as esbuild does) needs no Go and no download step. `@gojsop/cli` holds the launcher, `@gojsop/cli-<os>-<arch>` the binary, `@gojsop/testing` and `@gojsop/create` build on them; all are published by the release job with the version of the tag, pinned to each other, with provenance. The repository keeps `0.0.0`, so release-please touches no `package.json` and no lockfile. Not taken: a postinstall download, because it breaks offline installs and installs without scripts.
- **No breakpoint debugger in the script.** Status: accepted (2026-10-05). Why: QuickJS has no debug protocol; `--trace`, `console` and stack traces cover most needs. Test code itself is debugged in Node as usual.

## Open

WS-5, WS-8, WS-9, WS-10, WS-11
