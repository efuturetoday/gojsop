# Block: JS Registry

Status: accepted

## Decision

One `jsregistry.Registry` per process holds one persistent, long-lived JS VM per
`JSHook` / `JSAdmission` key (`types.NamespacedName`). All execution of user JS
at runtime goes through `Registry.Call(ctx, key, fn)`, which takes the per-VM
lock, recovers panics and classifies the result. Controllers only build, restart
and drop VMs; dispatcher and admission server only call.

Why (from `git log -- internal/jsregistry`):
- `61d1fe4` "ctx-aware VM and unified Registry.Call primitive": dispatcher and
  admission previously each reimplemented lock + recover + error classification.
  The former TODO.md said the same (now REG-6).
- `25d4744` "per-key build lock, no user JS under r.mu": a build runs user JS and
  must not block other keys. Reason beyond the commit title: not recorded.
- `025a482` "rich restart bookkeeping": per-reason counters and history ring on
  the VM. Reason not recorded beyond the commit title.
- Rejected alternatives: not recorded in git or the former TODO.md.

## Code

- Entry point for runtime execution: `Registry.Call` (`internal/jsregistry/registry.go:391`).
  It returns `(CallResult, *ManagedVM, error)`; `ErrUnknownKey` if the key is absent.
  Outcomes: `OK`, `Panic`, `Cancelled`, `MemoryLimit`, `Error` (switch at `registry.go:405-421`).
  `CallResult.Duration` includes lock-wait time on purpose (doc at `registry.go:386-389`).
- Entry point for building: `Registry.GetOrLoad` (`registry.go:230`) from controllers.
  Build is `Registry.build` (`registry.go:179`); the only `jsengine.New` call outside
  `internal/jsengine` is `registry.go:180`.
- What it holds: `Registry{mu, vms, buildLocks}` (`registry.go:143`). `ManagedVM`
  (`registry.go:79`) carries `VM`, `Opts` (cached `BuildOptions`, so a rescue can
  rebuild without the controller), `StartedAt`, `RestartsByReason`, `History`
  (ring, `historyCap`), `Extra` (opaque, e.g. parsed JSHook config), `CallMu`.
- Build-time hook: `PostBuildHook` (`registry.go:46`) runs under the key's build lock.
  Controllers use it to check required exports: JSHook `internal/jshook/controller/controller.go:111,114`,
  JSAdmission `internal/jsadmission/controller/controller.go:94-102` (`MissingExportError`).
- Errors: sentinels in `internal/jsregistry/errors.go`; mapped to conditions/events
  in `internal/conditions/conditions.go:65-76`.
- Callers of `Registry.Call`: dispatcher `internal/jshook/dispatcher/dispatcher.go:450`
  (runs `jshook.Handle`, `internal/jshook/handle.go:18`), admission
  `internal/jsadmission/server.go:249`. No other caller (checked by grep).
- Controllers: `GetOrLoad` at `internal/jshook/controller/controller.go:172` and
  `internal/jsadmission/controller/controller.go:152`; `RestartByKey` with `ReasonManual`
  at `jshook/controller/controller.go:201` and `jsadmission/controller/controller.go:178`;
  `Drop` on NotFound at `jshook/controller/controller.go:143`, `jsadmission/controller/controller.go:121`.
- Sources: `internal/jssource` (`Loader` interface and `Chain`, `loader.go:16-28`; `Hash`, `loader.go:43`).
  The controller loads bytes (`jshook/controller/controller.go:151`), hashes them (`:158`) and passes
  `SourceHash` in `BuildOptions`; the registry never loads sources itself. A changed hash makes
  `GetOrLoad` rebuild (`registry.go:232-247`, reason `ReasonSourceChanged`).
- Restart and lifecycle: reasons `ReasonSourceChanged|MemoryLimit|Panic|Timeout|TimeoutStreak|Manual`
  (`registry.go:31-36`). `Registry.RestartByKey` (`registry.go:353`) rebuilds from cached `Opts`.
  `jslifecycle.Rescue` (`internal/jslifecycle/jslifecycle.go:54`) wraps it and emits
  `Restarted` / `RescueFailed` events; callers `jsadmission/server.go:271,279,299`,
  `jshook/dispatcher/dispatcher.go:523`.
- Concurrency (`registry.go:140-146`, `:230-274`): `r.mu` guards only the two maps and is never held while
  user JS runs. Per-key build mutex serializes `GetOrLoad` and `RestartByKey` for one key. Per-VM `CallMu`
  serializes calls (the qjs runtime is not goroutine-safe, `registry.go:96-99`). Replacing or dropping a VM
  takes the old VM's `CallMu` before `VM.Close()` (`registry.go:262,329,372`).
- Wiring: one registry created in `cmd/main.go:241`; controllers fall back to `NewRegistry()` when nil
  (`jshook/controller/controller.go:342`, `jsadmission/controller/controller.go:326`).

## Rules

- Do: run user JS at runtime only inside the `fn` of `Registry.Call`; switch on `CallResult.Outcome`
  and call `jslifecycle.Rescue` for panic, memory-limit, timeout.
- Do: pass a ctx with a deadline to `Call`; cancellation relies on it.
- Do: put per-VM cached state in `ManagedVM.Extra` via `PostBuild`, not by calling into the VM from a reconcile.
- Don't: touch `ManagedVM.VM` or `CallMu` outside `internal/jsregistry` (the field is exported but this is the rule).
- Don't: call `jsengine.New` outside the registry.
- Don't: hold `r.mu` while running user JS or closing a VM.
- Don't: load sources inside the registry; controllers resolve source and hash first.

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| VM persists across loads | `go test ./internal/jsregistry -run TestRegistry_PersistsAcrossLoads` | yes (`make test` in `.github/workflows/test.yml`) |
| Source change rebuilds | `-run TestRegistry_RestartOnSourceChange` | yes |
| Restart from cached opts, unknown key | `-run 'TestRegistry_RestartByKey_RebuildsFromCachedSource\|TestRegistry_RestartByKey_UnknownHook'` | yes |
| Restart history ring and counters | `-run TestRegistry_RestartHistory_RingAndCounters` | yes |
| Get / Drop | `-run 'TestRegistry_Get\|TestRegistry_Drop'` | yes |
| Rescue events | `go test ./internal/jslifecycle` (`TestRescue_Success_EmitsRestarted`, `TestRescue_Failure_EmitsRescueFailed`, `TestRescue_NilEmitter_NoOps`) | yes |
| Lint (`errcheck`, `staticcheck`, `govet`, `unused`, ...) | `make lint` (`.golangci.yml`; `.github/workflows/lint.yml`) | yes |
| `Registry.Call` outcomes (ok, panic, cancelled, OOM, error, unknown key) | none: no test references `Call` (grep over `*_test.go`) | missing. Add `TestRegistry_Call_*` in `internal/jsregistry/registry_test.go`. |
| Call serialization and race safety | none | missing. Add a test with N goroutines on `Call` plus a concurrent `RestartByKey`, run with `go test -race ./internal/jsregistry`; add `-race` to `make test`. |
| No engine use bypassing `Registry.Call` | holds today (only `registry.go:180` calls `jsengine.New`; only `dispatcher.go:450` and `server.go:249` call `Registry.Call`) | missing. Add `depguard` in `.golangci.yml` denying `internal/jsengine` outside `internal/jsregistry`, `internal/jshook`, `internal/jsadmission`; or an arch test scanning imports. |
| `Drop` / `RestartByKey` close VM only after `CallMu` | none | missing. Covered by the race test above. |

Gates marked `missing` are commitments, not facts.

## Open

Tracked in [backlog](../backlog.md): REG-1 to REG-6; gates GATE-3, GATE-4.
