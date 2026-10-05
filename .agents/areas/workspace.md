---
id: workspace
status: proposed
---

# Workspace

This area describes what a maintainer who writes hooks and policies can do
on their own machine with the `gojsop` CLI: run a script and test it
without a cluster, with the same result the cluster would give.

The maintainer keeps hooks and policies in a repository of their own, one
directory each: the manifest (`hook.yaml` or `policy.yaml`, a `JSHook` or
`JSAdmission`), the script next to it, and test cases in `tests/`. The CLI
is one Go binary, released with the operator under the same version.

The script always runs in the engine the operator uses, with the same
`kube.*` surface, rights and limits; only the cluster is replaced by one
in memory. There is no Node.js mode: a test that passes in Node says
nothing about QuickJS (async, globals, language features, speed, limits,
the JSON border), so the CLI never runs a script anywhere else.

gojsop sets a contract, not a toolchain: in the end it takes one
JavaScript file. Any bundler may produce it; the CLI checks it.

Words used here:

- **case**: one test in a YAML file: an admission request or a hook event,
  the cluster before, and what must hold after.
- **fake cluster**: the objects a case lists, held in memory; `kube.*` and
  `event.all()` read and write them.

## Use cases

### workspace.UC1 Test a policy without a cluster

- **Actor**: policy maintainer
- **Trigger**: runs `gojsop test` in the workspace or on one directory
- **Before**: `policy.yaml` and its script; cases in `tests/*.yaml`
- **Steps**:
  1. The CLI prepares the script as the operator does and runs every case: a request, the cluster before, the expected answer (`allowed`, `message`, `warnings`, the object after a mutation, or an error).
  2. It prints one line per case and a diff for each failure.
- **Exceptions**: a script that throws, runs past its timeout or its memory limit fails the case unless the case expects that (workspace.R5).
- **Result**: the exit code is 0 only when every case passed (workspace.R7).

### workspace.UC2 Test a hook without a cluster

- **Actor**: hook maintainer
- **Trigger**: runs `gojsop test`
- **Before**: `hook.yaml` and its script; cases with an event and the cluster before
- **Steps**:
  1. The CLI calls `handle(event)`; `kube.*` and `event.all()` work on the fake cluster.
  2. The case compares the fake cluster after the call with what it expects.
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
| workspace.R1 | The CLI runs a script through the same engine, registry and `kube.*` binder as the operator; only the dynamic client is a fake. | decision "engine only" | `TestRun_UsesTheOperatorsEngineAndKubeSurface` |
| workspace.R2 | The script comes from `spec.source.inline`, or else from the one `.js` file next to the manifest. | decision "contract, not toolchain" | `TestLoad_ScriptInlineOrBesideTheManifest` |
| workspace.R3 | `kube.*` in a run or a case has exactly the rights of `spec.permissions` (and, for a hook, read on its bindings); anything else throws `Forbidden`. | kube-access.R11 | `TestFakeCluster_EnforcesPermissions` |
| workspace.R4 | A policy case checks `allowed`, `message` (exact text, or a regular expression between slashes), `warnings`, and for a mutating policy the object after the patch, compared as a subset. | decision "YAML cases" | `TestCase_PolicyExpectations` |
| workspace.R5 | A case passes when the script throws, times out or hits its memory limit only if it expects that error; the limits are the manifest's. | js-execution.R3, js-execution.R5 | `TestCase_ErrorsAndLimits` |
| workspace.R6 | A hook case checks the fake cluster after the call against expected objects, compared as a subset; `event.all()` returns the case's objects of the binding, filtered by its selectors. | jshook.R26 | `TestCase_HookChangesTheFakeCluster` |
| workspace.R7 | `gojsop test` prints one line per case and a diff per failure, and exits non-zero when any case fails. | decision "YAML cases" | `TestTestCommand_ExitCodeAndOutput` |
| workspace.R8 | `gojsop run --trace` prints every `kube.*` call with its arguments and its answer or error. | decision "no debugger" | `TestRun_TraceShowsKubeCalls` |

Every rule is held by a test or is `missing → <KEY>`.

## Aspects

- [js-execution](../aspects/js-execution.md): the engine, limits and the entry point contract the CLI shares.
- [js-registry](../aspects/js-registry.md): how the CLI prepares and calls a script, as the operator does.
- [kube-access](../aspects/kube-access.md): the `kube.*` surface and the rights the fake cluster enforces.

## Decisions

- **Scripts run only in the operator's engine, never in Node.** Status: accepted (2026-10-05). Why: Node differs in async, globals, language features, speed, limits and the JSON border, so a green Node test proves nothing; Cloudflare left its Node emulation (Miniflare) for its real runtime for the same reason. Not taken: a Node debug mode, because it would be a second truth.
- **gojsop sets a contract, not a toolchain.** Status: accepted (2026-10-05). Why: teams already have bundlers and test runners. The contract: one ES2023 script, the entry point a global function, no Node APIs, no `async` entry point. `gojsop build` may bundle TypeScript with an embedded esbuild for teams without a toolchain (WS-6). Not taken: requiring esbuild or a Node toolchain.
- **Test cases are YAML first.** Status: accepted (2026-10-05). Why: a case is mostly data (an object, an event), readable for policy authors, as in Kyverno. vitest and jest come through `@gojsop/testing`, which still runs in the engine (WS-7). Not taken: tests only in a JS framework.
- **No breakpoint debugger.** Status: accepted (2026-10-05). Why: QuickJS has no debug protocol; `--trace`, `console` and stack traces cover most needs. Not taken: debugging in Node, see the first decision.

## Open

WS-5, WS-6, WS-7, WS-8, WS-9
