---
id: jshook
status: proposed
entrypoints:
  - jshook.ReadConfig
  - jshook.Handle
  - jshook.BindingContext
  - dispatcher.Dispatcher.Subscribe
  - dispatcher.Dispatcher.Drop
---

# JSHook

This area describes what an engineer who writes a JSHook in JavaScript can
rely on: how the hook says what it watches, and when and with what gojsop
calls its `handle()`.

gojsop follows the shell-operator model. A hook exports two functions.
`config()` returns the bindings: what the hook wants to react to. A binding of
kind `kubernetes` names a resource and optional selectors. `handle(contexts)`
does the work and receives a BindingContext, a plain object that says which
binding fired and what changed. The author does not run a controller; they
write the JavaScript, apply a `JSHook` resource and read its status.

A binding first produces one `Synchronization` context with all matching
objects that exist when the watch starts. After that it produces `Event`
contexts, one per change (`Added`, `Modified`, `Deleted`). The hook decides
which of these it receives with `executeHookOnSynchronization` and
`executeHookOnEvent`. Only `kubernetes` bindings run today; the other
shell-operator fields are accepted but have no effect (see Open).

This area sits on top of the shared way gojsop runs JavaScript: limits,
timeouts and restarts are described in the aspects below, status and events
in [status-conditions](../aspects/status-conditions.md). The neighbour area
[jsadmission](jsadmission.md) uses the same engine for synchronous admission
instead of events.

## Use cases

### jshook.UC1 Declare what a hook watches

- **Actor**: hook author
- **Trigger**: applies a `JSHook` whose source exports `config()` and `handle()`
- **Before**: the source loads (inline or ConfigMap, see [js-sources](../aspects/js-sources.md))
- **Steps**:
  1. The author returns `{ kubernetes: [{ name, apiVersion, kind, ... }] }` from `config()`.
  2. gojsop reads `config()` once when it builds the hook's VM.
  3. gojsop starts watching each declared binding and reports the bindings in `status.bindings`.
- **Exceptions**: a missing `config` or `handle` export, a throwing `config()` or a non-JSON result fails the build and the hook reports it in its status (jshook.R2, jshook.R18). A changed `config()` takes effect only after a rebuild (jshook.R1).
- **Result**: the hook is subscribed to exactly the declared bindings.

### jshook.UC2 Receive the current state, then changes

- **Actor**: hook author
- **Trigger**: the watch of a binding starts (hook created, source or generation changed, VM rebuilt)
- **Before**: UC1 succeeded
- **Steps**:
  1. `handle()` is called once with a `Synchronization` context whose `objects` holds every matching object.
  2. Afterwards `handle()` is called with `Event` contexts, one per change, carrying `binding`, `type`, `watchEvent` and `object`.
- **Exceptions**: with `executeHookOnSynchronization: false` each existing object arrives as `Added` instead (jshook.R5). Changes that happen while the snapshot is built are not lost (jshook.R3, jshook.R4).
- **Result**: the hook sees a consistent start state and then strictly the deltas.

### jshook.UC3 Choose which events and objects reach the hook

- **Actor**: hook author
- **Trigger**: writes `executeHookOnEvent` and selectors in a binding
- **Before**: UC1
- **Steps**:
  1. The author lists event types (`Added`, `Modified`, `Deleted`) or leaves the list out to get all.
  2. The author narrows objects with a namespace and `labelSelector.matchLabels`.
  3. Only matching objects and listed event types reach `handle()`.
- **Exceptions**: selector forms gojsop does not apply are listed under Open; they are not silently half applied (jshook.R15, jshook.R16).
- **Result**: `handle()` runs only for what the author asked for.

### jshook.UC4 Handle a burst of changes

- **Actor**: hook author
- **Trigger**: one object changes many times faster than `handle()` runs
- **Before**: UC2
- **Steps**: gojsop folds the waiting changes of that object into one call that carries the newest object.
- **Exceptions**: none; changes of different objects stay separate calls.
- **Result**: the hook never sees stale state of an object it is about to receive again, and its calls never overlap (jshook.R8, jshook.R9).

### jshook.UC5 Recover from a failing hook

- **Actor**: hook author
- **Trigger**: `handle()` throws, exceeds `timeoutSeconds`, panics the engine or exceeds the memory limit
- **Before**: the hook runs
- **Steps**:
  1. gojsop records a Warning event on the `JSHook` and logs the cause.
  2. A thrown error: the event is retried later with backoff; the VM stays.
  3. A timeout: the call is cancelled and the VM is restarted at once (it is dead after the cancellation); the event is retried.
  4. A panic or memory-limit error: the VM is restarted at once and the event is retried.
  5. The restart builds in the background. Until the new VM is ready the hook shows `Ready=False` (`Building`, then `BuildFailed` if it fails) and the events wait (jshook.R18, jshook.R19).
- **Exceptions**: a hook that fails forever is retried forever (see Open).
- **Result**: the hook keeps receiving events on a healthy VM; the restart is visible as an event and in status ([js-registry](../aspects/js-registry.md)).

### jshook.UC6 Remove a hook

- **Actor**: hook author
- **Trigger**: deletes the `JSHook` resource
- **Steps**: the subscription is dropped, then the VM.
- **Result**: no further `handle()` calls (jshook.R14).

## Rules

| ID | Rule | Source | Held by |
|---|---|---|---|
| jshook.R1 | `config()` is read once per VM build and its result becomes the hook's bindings; a hook without `config()` yields no bindings. | `jshook.ReadConfig`, `jshook.Config` | `TestReadConfig_FromHookSource`, `TestReadConfig_MissingFunction` |
| jshook.R2 | A hook that exports no `config` or no `handle` fails to build and reports it. | `jshook.ReadConfig`, controller `readConfig` | `TestReadConfig_PostBuildRejectsMissingExports`, `TestControllers` |
| jshook.R3 | A binding delivers its `Synchronization` context first; changes that arrive while it is prepared are delivered after it, not dropped. | `dispatcher.subscription.startWatcher` | `TestDispatcher_SynchronizationFirstThenDeltas` |
| jshook.R4 | An `Added` event for an object that already is in the snapshot is not delivered again. | `dispatcher.subscription.startWatcher` | `TestDispatcher_SynchronizationFirstThenDeltas` |
| jshook.R5 | With `executeHookOnSynchronization: false` no `Synchronization` context is sent and each existing object arrives as `Added`. | `jshook.KubernetesBinding.ExecuteHookOnSynchronization` | `TestDispatcher_SyncDisabled_ExistingObjectsArriveAsAdded` |
| jshook.R6 | Only event types listed in `executeHookOnEvent` are delivered; an empty list means all three. | `jshook.KubernetesBinding.ExecuteHookOnEvent` | `TestDispatcher_ExecuteHookOnEvent_ListedTypesOnly`, `TestDispatcher_ExecuteHookOnEvent_EmptyMeansAll` |
| jshook.R7 | `handle()` receives one BindingContext per call, as a one-element array: `Event` with `binding`, `type`, `watchEvent`, `object`; `Synchronization` with `binding`, `type`, `objects`. | `jshook.BindingContext`, `jshook.Handle` | `TestHandle_ReceivesBindingContext` |
| jshook.R8 | Changes of one object that wait for a call fold into one call with the newest object. | shell-operator parity, `dispatcher.eventKey` | `TestDispatcher_BurstOfChangesFoldsIntoNewestObject` |
| jshook.R9 | Calls of one hook never overlap, also not across a re-subscribe. | decision (commit history), `dispatcher.subscription.stop` | `TestDispatcher_ResubscribeDuringCall_NoOverlapAndProcessAlive` |
| jshook.R10 | A throwing `handle()` records a Warning event, does not restart the VM and the event is retried with backoff. | `dispatcher.subscription.handleEvent` | `TestDispatcher_ThrowingHandle_WarnsRetriesWithoutRestart` |
| jshook.R11 | A call is stopped at the hook's `timeoutSeconds` and records a Warning event; the VM keeps its top-level state and is not restarted, and the event is retried. | `jsengine.Limits`, `dispatcher.subscription.handleEvent` | `TestDispatcher_Timeout_CancelsWarnsAndKeepsVM` |
| jshook.R12 | A panic (a wasm trap) starts the VM restart at once and the event is retried. A memory-limit error records a Warning, keeps the VM and the event is retried. | [js-registry](../aspects/js-registry.md), REG-4, `dispatcher.subscription.handleEvent` | `TestDispatcher_MemoryLimit_WarnsKeepsVMAndRetries`, `TestDispatcher_PanicInHandle_RestartsVMAndRetries` |
| jshook.R13 | A retried event never overwrites a fresher state of the same object; success ends the retries. | `dispatcher.subscription.requeue` | `TestDispatcher_RetryKeepsFresherStateOfSameObject`, `TestDispatcher_ThrowingHandle_WarnsRetriesWithoutRestart` |
| jshook.R14 | After the hook is deleted it receives no more events. | `dispatcher.Dispatcher.Drop` | `TestDispatcher_Drop_NoMoreEvents` |
| jshook.R15 | A namespace (one name) and `labelSelector.matchLabels` restrict the objects delivered. | `jshook.KubernetesBinding` | `TestControllers` |
| jshook.R16 | A binding field that gojsop does not act on is not presented as working (project rule API-1). Violated today: `schedule`, `onStartup`, `jqFilter`, `allowFailure`, `queue`, other selector forms. | decision API-1 | missing → DISP-1 |
| jshook.R17 | A hook whose informer does not sync does not block Subscribe or Drop of other hooks; Drop of that hook cancels its pending Subscribe. | `dispatcher.Dispatcher.Subscribe`, `dispatcher.Dispatcher.Drop` | `TestDispatcher_SlowSync_DoesNotBlockOtherHooks` |
| jshook.R18 | While the VM is not ready the hook is `Ready=False` with reason `Building` (the build runs; the reconcile does not wait for it) or `BuildFailed` (the last build failed; retried with backoff, a source change rebuilds at once). | [js-registry](../aspects/js-registry.md), status-conditions.R7 | `TestReconcile_HangingBuildDoesNotBlockOtherHook`, `TestReconcile_BrokenBuild_BacksOffAndSourceChangeRebuildsAtOnce` |
| jshook.R19 | While the hook has no VM (a restart builds, or the build failed) its events are kept and retried with the rate limiter, and reach the new VM; none is dropped. | [js-registry](../aspects/js-registry.md), js-registry.R19, `dispatcher.subscription.noVM` | `TestDispatcher_NoVM_KeepsEventsAndDeliversAfterRebuild` |

Every rule is held by a test or is `missing → <KEY>`.

## Aspects

- [js-execution](../aspects/js-execution.md): limits, timeouts and memory of the VM that runs `handle()`.
- [js-registry](../aspects/js-registry.md): one VM per hook behind `jsrun.Runner`, restart and rescue.
- [js-sources](../aspects/js-sources.md): where the hook source comes from.
- [kube-access](../aspects/kube-access.md): how the dispatcher and hooks reach the cluster.
- [status-conditions](../aspects/status-conditions.md): conditions and events a failing or restarted hook shows.
- [api-design](../aspects/api-design.md): the rule that a field needs an implementation (API-1).

## Decisions

- **One subscription per hook, with one informer per kubernetes binding, one rate-limited FIFO workqueue and one worker goroutine.** Status: accepted (commit history, no date or name recorded). Why: calls must not overlap and the VM rescue after a call is owned by one goroutine. Not taken: a worker per binding, because it breaks R9; reasons for others are not recorded.
- **The queue holds only `eventKey` struct values; payloads sit in a pending map.** Status: accepted (commit history). Why: BindingContext is not hashable, equal keys fold bursts (R8).
- **`Subscribe` waits for the old worker before it starts the new one, under a per-hook lock; the dispatcher lock only guards the subscription map.** Status: accepted. Why: a slow cache sync of one hook must not block other hooks (R17); the old worker must still be gone before the new one starts (R9).
- **Subscribe only after a VM (re)start or a generation change; a rescue does not re-subscribe.** Status: accepted (commit history). Why: the worker looks up the live VM per call. Consequence: a changed `config()` after a rescue is not applied (DISP-9).
- **Failed calls are requeued with the default controller rate limiter and no retry cap.** Status: accepted (commit history). Known debt: DISP-10.
- **Namespace selection is a single-namespace fast path.** Status: accepted as MVP; reason not recorded beyond the code comment (DISP-11).

## Open

DISP-1, DISP-2, DISP-3, DISP-4, DISP-5, DISP-6, DISP-7, DISP-9, DISP-10, DISP-11, STAT-5, API-1, REG-4, EXEC-2
