# Block: Hook Dispatch

Status: accepted

## Decision

A JSHook declares what it wants in an exported `config()` (shell-operator
vocabulary). The controller reads it once per VM build, and a per-hook
`dispatcher.subscription` turns Kubernetes informer events into `handle([bindingContext])`
calls. One subscription per hook has one rate-limited workqueue and one FIFO worker
goroutine, so `handle()` calls for a hook never overlap. The worker runs JS only through
`Registry.Call` (see [js-registry](js-registry.md)); this block does not repeat registry internals.

Why (from `git log -- internal/jshook/dispatcher`):
- `8a81614` "emit Synchronization context on Subscribe": shell-operator parity, one snapshot
  before deltas (comment `dispatcher.go:383-385`).
- `0eea4d8` "buffer pre-sync events, struct keys, graceful shutdown": never drop events that
  arrive before the snapshot (`dispatcher.go:266-272`); struct keys collapse bursts per object
  (`:53-57`); `stop()` waits for the worker so re-subscribe cannot overlap a running call (`:196-199`).
- `61d1fe4` moved lock, recover and OOM/cancel classification into `Registry.Call`; the dispatcher
  keeps only the policy table (`:420-427`).
- Rejected alternatives: not recorded.

## Code

- `config()` decode: `jshook.ReadConfig` (`internal/jshook/config.go:19`). Missing export gives
  `(nil, nil)`; empty result gives `(nil, nil)`; non-JSON gives an error. It runs with the build ctx,
  not the per-call timeout (`config.go:17-18`). Unknown JSON fields are ignored (`config_types.go:3-5`).
- Types (`config_types.go`): `Config{ConfigVersion, OnStartup int, Schedule, Kubernetes}` (`:18`);
  `ScheduleBinding{Name, Crontab, AllowFailure, Queue}` (`:25`); `KubernetesBinding` (`:32`) with
  `ExecuteHookOnEvent []string`, `ExecuteHookOnSynchronization *bool`, `NameSelector`, `LabelSelector`,
  `FieldSelector`, `Namespace`, `JQFilter`, `AllowFailure`, `Queue`.
- Build-time check: `readConfig` PostBuildHook requires exports `config` and `handle`
  (`controller/controller.go:109-121`) and stores `*jshook.Config` in `ManagedVM.Extra`;
  `configFromExtra` reads it back (`:123`). A nil config fails the reconcile (`:217-222`).
- Binding kinds:
  - `kubernetes`: the only kind that runs. `Subscribe` loops over `cfg.Kubernetes` (`dispatcher.go:134`).
  - `schedule` and `onStartup`: decoded and listed by `summarizeBindings` (`controller.go:311-322`),
    never executed. grep finds no other reader of `Schedule` or `OnStartup`.
- Dispatcher: `New` (`dispatcher.go:94`), `Subscribe` (`:108`), `Drop` (`:158`), one `subscription` per
  hook key (`:175`). Per binding: REST mapping (`:140`), `startWatcher` (`:206`), one filtered dynamic
  informer each (`:226-229`). Then one worker `runWorker` (`:152`, `:399`).
- Queue: `workqueue` keyed by `eventKey{binding,event,namespace,name,uid}` (`:59`); the payload sits in
  `pending map[eventKey]BindingContext` under `pendMu` (`:182-183`). A new event for the same key replaces
  the payload, so a burst collapses into one call with the freshest object (`:172-174`, `:260-263`).
- Synchronization versus event:
  1. Handlers are gated; events before the gate are buffered (`:277-295`).
  2. After `WaitForCacheSync` (`:307`) the store is listed (`:314`). If `executeHookOnSynchronization` is
     nil or true, one `Type:"Synchronization"` context with all objects is queued (`:311`, `:327`, `:386-397`).
  3. If false, each existing object is queued as an `Added` event instead (`:330-334`).
  4. The buffer drains in order; `Added` redeliveries of UIDs already in the snapshot are dropped (`:352-360`).
- Event filter: empty `executeHookOnEvent` means Added, Modified, Deleted (`:231-238`); other events are
  dropped in `enqueueEvent` (`:246`). Event names are matched as raw strings, unknown names never match.
- Selectors actually applied: namespace only when `namespace.nameSelector.matchNames` has exactly one
  entry (`:208-211`, comment "MVP"); label selector only from `labelSelector.matchLabels` with string
  values (`:213-224`). Ignored: top-level `nameSelector`, `fieldSelector`, `namespace.labelSelector`,
  `matchExpressions`, multi-namespace lists, `jqFilter` (grep: no reader).
- BindingContext (`internal/jshook/bindingctx.go:9`): `binding` (name, falls back to `kind`, `dispatcher.go:240-243`),
  `type`, `watchEvent`, `object`, `filterResult`, `objects []SyncObject`, `snapshots`. Event contexts set `object`
  and `watchEvent`; Synchronization sets `objects`. `filterResult` and `snapshots` are never set. Type
  `"Schedule"` is documented in the comment (`:11`) but never produced.
- Execution: `handleEvent` (`dispatcher.go:436`) resolves the VM with `reg.Get` (`:437`), derives the budget from
  `VM.Limits().TimeoutSeconds` (`:445`), wraps it with `contextWithOptionalTimeout` (`:446`, `:512`) and calls
  `Registry.Call` with `jshook.Handle` (`:450-453`, `internal/jshook/handle.go:18`), one context per call as a
  one-element array. Outcomes (`:462-507`):
  - Panic or memory limit: `rescue` then `requeue`.
  - Cancelled (deadline): `timeoutStreak++`, Warning event `EventHandleTimeout`, rescue with
    `ReasonTimeoutStreak` only at `timeoutStreakThreshold = 3` (`:51`, `:484`), then requeue.
  - Error: Warning `EventHandleFailed`, streak reset, requeue.
  - OK: streak reset, `Forget`.
  - `requeue` keeps the old payload only if no fresher one is pending (`:541-548`); retry is `AddRateLimited`
    with the default controller rate limiter and no retry cap.
  - `rescue` goes through `jslifecycle.Rescue` (`:523`) and resets the streak on success only.
- Controller wiring: `Reconcile` calls `Dispatcher.Subscribe(r.subscribeCtx(), key, cfg, emit)` only when the VM
  was (re)started or `ObservedGeneration != Generation` (`controller.go:224-237`); failure becomes `EventSubscribeFailed`.
  `Dispatcher.Drop` runs on NotFound, before `Registry.Drop` (`:140-143`). `emit` is bound to a deep copy of the hook (`:228`).
  Wiring in `cmd/main.go:248,262-263` (`SubscribeCtx` is the manager context). `Dispatcher` may be nil (tests, `controller.go:74`).

## Rules

- Do: add a binding kind by extending `Config`, `Subscribe` and `summarizeBindings` together, and put a test next to it.
- Do: keep one worker per subscription; all state touched by `runWorker` (`timeoutStreak`) relies on it (`dispatcher.go:192`).
- Do: queue through `eventKey` plus `pending`; never put a `BindingContext` in the queue.
- Don't: call the VM from the dispatcher except through `Registry.Call`.
- Don't: re-subscribe from a rescue; the worker resolves the live VM per call and keeps the cfg from Subscribe time.
- Don't: advertise a binding field without a reader (API-1).

## Gates

| Gate | Command / test | Enforced in CI |
|------|----------------|----------------|
| `config()` decode, missing export gives nil | `go test ./internal/jshook -run 'TestReadConfig_FromHookSource\|TestReadConfig_MissingFunction'` | yes (`make test` runs `go test` over all packages except e2e) |
| `handle()` receives the BindingContext | `go test ./internal/jshook -run 'TestHandle_ReceivesBindingContext\|TestHandle_MissingFunction'` | yes |
| Status lists resolved bindings after reconcile | `test/integration/jshook_controller_test.go` "should reconcile and write resolved bindings into status" (envtest, no Dispatcher wired) | yes |
| Dispatcher: Synchronization first, then deltas, pre-sync buffer, UID dedupe | none: no test imports `dispatcher` | missing. Add `internal/jshook/dispatcher/dispatcher_test.go` with a fake dynamic client (`k8s.io/client-go/dynamic/fake`) and a stub `RESTMapper`. |
| Event filter and `executeHookOnSynchronization:false` replay | none | missing. Table test in the same file. |
| Timeout streak: 2 timeouts no rescue, 3rd rescues, success resets | none | missing. Test `handleEvent` with a fake registry VM that loops, `TimeoutSeconds: 1`. |
| Panic/OOM/error requeue and `Forget` on OK | none | missing. Same file. |
| `Subscribe` replacement does not overlap an in-flight call | none | missing. Test with `-race`. |
| Subscribe/Drop wiring in the controller | none (integration suite has no Dispatcher) | missing. Add an envtest case with a dispatcher and a real watched ConfigMap. |

Gates marked `missing` are commitments, not facts.

## Open

Tracked in [backlog](../backlog.md): DISP-1 to DISP-11, STAT-5, API-1, REG-4, EXEC-2, DOC-4; gates GATE-17.
