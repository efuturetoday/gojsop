# Block: JS Execution

Status: accepted

## Decision

User JavaScript runs in one persistent QuickJS runtime per JSHook/JSAdmission, compiled to WebAssembly and executed by wazero, through `github.com/fastschema/qjs` v0.0.6 (`go.mod:6`; wazero v1.9.0 is an indirect dependency, `go.mod:59`). The VM is long-lived: top-level state survives across calls (`internal/jsengine/vm.go:158`). Calls are cancelled by the Go context, not by QuickJS interrupts: `CloseOnContextDone: true` (`vm.go:72`), because qjs `MaxExecutionTime` is a no-op in v0.0.6 (`vm.go:27`). Memory is capped with `MemoryLimit`, stack at 1 MiB (`vm.go:52,69-70`).

Rejected or deferred, from code and the former TODO.md: forking qjs to get QuickJS-level interrupts (only a fallback if ctx-cancel overhead proves too high, EXEC-3). Reason for choosing qjs/wazero over other engines: not recorded. History: `64426b4` carved out `internal/jsengine`; `61d1fe4` made the VM ctx-aware and added `Registry.Call`; `7a14471` replaced the process-wide kube singleton with `kubehost.Factory`.

## Code

- Engine: `internal/jsengine/vm.go` (`New` :66, `Eval` :139, `LoadModule` :160, `HasExport` :172, `CallExport` :184, `Close` :215).
- Per-call cancellation: `withContext` (:104) swaps the Go context embedded in `qctx.Context` for the call and restores it. Exposed as `VM.WithContext` (:122) for callers that need raw qjs.
- Errors: `wrapEngineErr` (`oom.go:40`) is the only place that sniffs qjs/wazero error text. It checks `ctx.Err()` first (`ErrCancelled`), then "out of memory" (`ErrOOM`). `WrapEngineErr` is the exported form (`vm.go:130`).
- Limits: `jsengine.Limits{MemoryMB, TimeoutSeconds}` (`vm.go:20`); zero falls back to `DefaultLimits()` = 32 MB / 30 s (`vm.go:33,38`). CRD `spec.limits` maps in via `limitsFromSpec` (`internal/jshook/controller/controller.go:301`) and `admissionLimitsFromSpec` (`internal/jsadmission/controller/controller.go:303`). `TimeoutSeconds` is not enforced inside `jsengine`; callers build the deadline context. Admission does `context.WithTimeout` at `internal/jsadmission/server.go:245`. The JSHook dispatcher does the same in `contextWithOptionalTimeout` (`internal/jshook/dispatcher/dispatcher.go:512`; a zero timeout means no deadline, rationale in the comment at :428).
- Single entry point for running JS: `Registry.Call(ctx, key, fn)` (`internal/jsregistry/registry.go:391`). It takes `ManagedVM.CallMu` (:99), recovers panics, and classifies the outcome as OK / Cancelled / MemoryLimit / Error / Panic (:404-424). The `fn` receives the `*jsengine.VM` and calls `CallExport` or `WithContext`. Typed wrappers: `jshook.CallExport(ctx, "handle", ...)` (`internal/jshook/handle.go:19`), `config` (`internal/jshook/config.go:23`), `jsadmission.Handle` (`internal/jsadmission/handle.go:99`, via `WithContext`).
- VM creation and teardown: only in `registry.go:180-198` (New, BindHost, LoadModule, post-build). Every failure path calls `vm.Close()`. Restart and drop take the old VM's `CallMu` before `Close` (:262-264, :329-334, :372-374), because closing during an in-flight wasm call races in wazero (`vm.go:213`).
- Host APIs: the only host surface is `kube` on `globalThis`, bound by a `HostBinder` (`internal/jsengine/host.go:8`) before `LoadModule`. `KubeHost` binds `kube.apply/get/list/delete` for hooks (`kubehost/kubehost.go:52-61`). `ReadOnlyKubeHost` binds only `get/list` for admission (:80-87). `Factory.ForHook/ForAdmission` mints the binder (`kubehost/factory.go:24-25`). Nothing else (no `fetch`, `console`, timers, filesystem) is bound in this repo; whatever qjs provides by default is not verified here.
- Isolation between scripts: one `qjs.Runtime` per resource key (separate wasm instance and heap), one binder instance per VM, calls serialized per VM by `CallMu`. The registry map lock `r.mu` is never held while JS runs (`registry.go:137`).

## Rules

- Do: run user JS only through `Registry.Call`; never call `VM` methods outside that callback. Exception: build time inside `Registry.build` (`LoadModule`, `PostBuildHook` such as `config()`), which runs under the per-key build lock.
- Do: pass a deadline context derived from `spec.limits.timeoutSeconds` into every call.
- Do: add new host functions as a `HostBinder` under `internal/jsengine/kubehost` (or another subpackage of `jsengine`), bound before `LoadModule`.
- Do: classify engine errors with `errors.Is(err, jsengine.ErrCancelled/ErrOOM)`.
- Do: hold `CallMu` before `VM.Close()`.
- Don't: import `fastschema/qjs` or `wazero` outside `internal/jsengine/**`.
- Don't: string-match engine errors outside `oom.go`.
- Don't: mutate the embedded `qjs.Context.Context` directly (`vm.go:89-92`).
- Don't: bind write functions (`apply`, `delete`) into admission VMs.
- Don't: share one VM between two resources.

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| Eval works, state persists | `go test ./internal/jsengine -run 'TestEval_1Plus1\|TestEval_PersistentState'` | yes (`test.yml` runs `make test`) |
| Memory limit enforced and classified | `TestMemoryLimit_Honoured`, `TestWrapOOM`, `TestIsOOMError_OnlyMatchesSentinel` | yes |
| `DefaultLimits` match CRD kubebuilder tags | `TestDefaultLimits_MatchKubebuilderTags` | yes |
| Kube surface per binder (full vs read-only) | `TestSharedFactory_ForHook_FullSurface`, `TestSharedFactory_ForAdmission_ReadOnlySurface`, `TestSharedFactory_PerCallInstances`; `TestKubeHost_*` | yes |
| Registry lifecycle (persist, restart, drop) | `TestRegistry_*` in `internal/jsregistry` | yes |
| Lint (revive, staticcheck, errcheck, govet, ...) | `make lint` (`.golangci.yml`; `lint.yml`) | yes |
| ctx deadline yields `ErrCancelled` | missing: test in `internal/jsengine` running `for(;;){}` with a 100 ms deadline, asserting `errors.Is(err, ErrCancelled)` and that the VM is closable afterwards | missing |
| Import boundary: no qjs/wazero outside `internal/jsengine` | missing: `go list -deps`/`go/packages` test failing on those imports in other packages, or a golangci `depguard` rule with `internal/jsengine/**` allowed. Violated today, see Open. | missing |
| `Registry.Call` panic and outcome classification | missing: test with panic, cancelled, OOM `fn` results (no `Call` test among the `TestRegistry_*` names) | missing |
| Admission VM has no `kube.apply/delete` | partial: factory test covers the binder; missing an end-to-end check that `jsadmission` uses `ForAdmission` | missing |
| Isolation: global set in VM A not visible in VM B | missing: registry test with two keys | missing |

## Open

Tracked in [backlog](../backlog.md): EXEC-1, EXEC-2, EXEC-3, EXEC-4; gates GATE-2, GATE-5, GATE-6, GATE-7.
