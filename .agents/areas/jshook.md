---
id: jshook
status: proposed
entrypoints:
  - corev1alpha1.HookBinding
  - jshook.Handle
  - jshook.BindingContext
  - dispatcher.Dispatcher.Subscribe
  - dispatcher.Dispatcher.Drop
---

# JSHook

This area describes what an engineer who writes a JSHook in JavaScript can
rely on: how the hook says what it watches, and when and with what gojsop
calls its `handle()`.

A hook is one `JSHook` resource. Its `spec.bindings` say what it reacts to,
its `spec.source` holds a script that exports `handle(contexts)`.
`handle()` receives a BindingContext, a plain object that says which binding
fired and what changed. The author does not run a controller; they write the
JavaScript, apply a `JSHook` and read its status.

A binding names resources the way a webhook rule does — apiGroups,
apiVersions, resources — and narrows them with `namespaceSelector` and
`objectSelector`. That is the same shape `JSAdmission` uses
(api-design.R10), so a scope learned on one CRD reads on the other.

A binding first produces one `Synchronization` context with all matching
objects that exist when the watch starts. After that it produces `Event`
contexts, one per change (`Added`, `Modified`, `Deleted`). The hook decides
which of these it receives with `synchronization` and `events`.

This area sits on top of the shared way gojsop runs JavaScript: limits,
timeouts and restarts are described in the aspects below, status and events
in [status-conditions](../aspects/status-conditions.md). The neighbour area
[jsadmission](jsadmission.md) uses the same engine for synchronous admission
instead of events.

## Use cases

### jshook.UC1 Declare what a hook watches

- **Actor**: hook author
- **Trigger**: applies a `JSHook` with `spec.bindings` and a source that exports `handle()`
- **Before**: the source loads (inline or ConfigMap, see [js-sources](../aspects/js-sources.md))
- **Steps**:
  1. The author writes `spec.bindings: [{ name, apiGroups, apiVersions, resources, ... }]`.
  2. gojsop resolves every binding against the cluster's resources before it changes any watch.
  3. gojsop starts one watch per resource and reports them in `status.bindings`.
- **Exceptions**: a binding that names `*` or a resource the cluster does not serve, or two bindings that watch the same resource, fail the reconcile and leave the hook's existing watches alone (jshook.R20, jshook.R21). A source without `handle()` fails the build (jshook.R2, jshook.R18).
- **Result**: the hook is subscribed to exactly the declared bindings.

### jshook.UC2 Receive the current state, then changes

- **Actor**: hook author
- **Trigger**: the watch of a binding starts (hook created, source or generation changed, VM rebuilt)
- **Before**: UC1 succeeded
- **Steps**:
  1. `handle()` is called once with a `Synchronization` context whose `objects` holds every matching object.
  2. Afterwards `handle()` is called with `Event` contexts, one per change, carrying `binding`, `type`, `watchEvent` and `object`.
- **Exceptions**: with `synchronization: false` each existing object arrives as `Added` instead (jshook.R5). Changes that happen while the snapshot is built are not lost (jshook.R3, jshook.R4).
- **Result**: the hook sees a consistent start state and then strictly the deltas.

### jshook.UC3 Choose which events and objects reach the hook

- **Actor**: hook author
- **Trigger**: writes `events` and selectors in a binding
- **Before**: UC1
- **Steps**:
  1. The author lists event types (`Added`, `Modified`, `Deleted`) or leaves the list out to get all.
  2. The author narrows objects with `objectSelector` and their namespaces with `namespaceSelector`. A single namespace is selected by its name label, `kubernetes.io/metadata.name`.
  3. Only matching objects and listed event types reach `handle()`.
- **Exceptions**: a `namespaceSelector` never matches a cluster-scoped object, which has no namespace (jshook.R22). Every selector form of a `LabelSelector` is applied, including `matchExpressions` (jshook.R15).
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
| jshook.R1 | A hook's bindings come from `spec.bindings`; the script is not asked what it watches. Changing `spec.bindings` re-subscribes the hook without rebuilding its VM. | `corev1alpha1.JSHookSpec.Bindings`, api-design.R11 | `TestSummarizeBindings_ListsEveryWatchedResource`, `TestControllers` |
| jshook.R2 | A hook that exports no `handle` fails to build and reports it. | controller `requireHandle` | `TestRequireHandle_PostBuildRejectsMissingHandle`, `TestRequireHandle_AcceptsHandleOnly`, `TestControllers` |
| jshook.R3 | A binding delivers its `Synchronization` context first; changes that arrive while it is prepared are delivered after it, not dropped. | `dispatcher.subscription.startWatcher` | `TestDispatcher_SynchronizationFirstThenDeltas` |
| jshook.R4 | An `Added` event for an object that already is in the snapshot is not delivered again. | `dispatcher.subscription.startWatcher` | `TestDispatcher_SynchronizationFirstThenDeltas` |
| jshook.R5 | With `synchronization: false` no `Synchronization` context is sent and each existing object arrives as `Added`. | `corev1alpha1.HookBinding.WantsSynchronization` | `TestDispatcher_SyncDisabled_ExistingObjectsArriveAsAdded` |
| jshook.R6 | Only event types listed in `events` are delivered; an empty list means all three. | `corev1alpha1.HookBinding.WantsEvent` | `TestDispatcher_ExecuteHookOnEvent_ListedTypesOnly`, `TestDispatcher_ExecuteHookOnEvent_EmptyMeansAll` |
| jshook.R7 | `handle()` receives one BindingContext per call, as a one-element array: `Event` with `binding`, `type`, `watchEvent`, `object`; `Synchronization` with `binding`, `type`, `objects`. | `jshook.BindingContext`, `jshook.Handle` | `TestHandle_ReceivesBindingContext` |
| jshook.R8 | Changes of one object that wait for a call fold into one call with the newest object. | shell-operator parity, `dispatcher.eventKey` | `TestDispatcher_BurstOfChangesFoldsIntoNewestObject` |
| jshook.R9 | Calls of one hook never overlap, also not across a re-subscribe. | decision (commit history), `dispatcher.subscription.stop` | `TestDispatcher_ResubscribeDuringCall_NoOverlapAndProcessAlive` |
| jshook.R10 | A throwing `handle()` records a Warning event, does not prepare the script again and the event is retried with backoff. | `dispatcher.subscription.handleEvent` | `TestDispatcher_ThrowingHandle_WarnsRetriesWithoutRestart` |
| jshook.R11 | A call is stopped at the hook's `timeoutSeconds` and records a Warning event; the script is not prepared again, and the event is retried. | `jsengine.Limits`, `dispatcher.subscription.handleEvent` | `TestDispatcher_Timeout_CancelsWarnsAndKeepsVM` |
| jshook.R12 | A panic (a wasm trap) or a memory-limit error ends only that call: the event is retried on a fresh VM of the same script, nothing is prepared again. A memory-limit error records a Warning. | [js-registry](../aspects/js-registry.md), js-registry.R2, `dispatcher.subscription.handleEvent` | `TestDispatcher_MemoryLimit_WarnsKeepsVMAndRetries`, `TestDispatcher_PanicInHandle_RetriesOnFreshVM` |
| jshook.R13 | A retried event never overwrites a fresher state of the same object; success ends the retries. | `dispatcher.subscription.requeue` | `TestDispatcher_RetryKeepsFresherStateOfSameObject`, `TestDispatcher_ThrowingHandle_WarnsRetriesWithoutRestart` |
| jshook.R14 | After the hook is deleted it receives no more events. | `dispatcher.Dispatcher.Drop` | `TestDispatcher_Drop_NoMoreEvents` |
| jshook.R15 | `objectSelector` and `namespaceSelector` restrict the objects delivered, in every `LabelSelector` form. | `corev1alpha1.ObjectMatch`, `dispatcher.Dispatcher.planWatches` | `TestControllers` |
| jshook.R16 | Every field of a binding is acted on; `spec.bindings` carries no field gojsop ignores (project rule API-1). | decision API-1, api-design.R6 | `TestSummarizeBindings_ListsEveryWatchedResource` |
| jshook.R17 | A hook whose informer does not sync does not block Subscribe or Drop of other hooks; Drop of that hook cancels its pending Subscribe. | `dispatcher.Dispatcher.Subscribe`, `dispatcher.Dispatcher.Drop` | `TestDispatcher_SlowSync_DoesNotBlockOtherHooks` |
| jshook.R18 | While the VM is not ready the hook is `Ready=False` with reason `Building` (the build runs; the reconcile does not wait for it) or `BuildFailed` (the last build failed; retried with backoff, a source change rebuilds at once). | [js-registry](../aspects/js-registry.md), status-conditions.R7 | `TestReconcile_HangingBuildDoesNotBlockOtherHook`, `TestReconcile_BrokenBuild_BacksOffAndSourceChangeRebuildsAtOnce` |
| jshook.R19 | While the hook has no prepared script (its build runs, or the build failed) its events are kept and retried with the rate limiter, and reach the script once it is prepared; none is dropped. | [js-registry](../aspects/js-registry.md), js-registry.R19, `dispatcher.subscription.noVM` | `TestDispatcher_NoVM_KeepsEventsAndDeliversAfterBuild` |
| jshook.R20 | A binding that cannot be resolved fails the reconcile before any watch is touched: the hook keeps the watches it already has. | `dispatcher.Dispatcher.Subscribe` | `TestPlanWatches_BadBindingKeepsExistingWatches` |
| jshook.R21 | A hook binding names a concrete apiGroup, apiVersion and resource. `*` is rejected, as is a resource the cluster does not serve or a resource two bindings of the same hook watch. | `dispatcher.Dispatcher.planWatches`, api-design.R10 | `TestPlanWatches_RejectsWildcardUnknownAndDuplicate` |
| jshook.R22 | `namespaceSelector` matches the labels of the object's namespace, read from one namespace cache shared by all hooks. A cluster-scoped object has no namespace and matches only when the selector is absent. | `dispatcher.Dispatcher.namespaceMatcher` | `TestControllers` |
| jshook.R23 | `status.bindings` lists one entry per binding and watched resource, `name:group/version/resource`. Every entry is a watch that runs. | controller `summarizeBindings` | `TestSummarizeBindings_ListsEveryWatchedResource` |

Every rule is held by a test or is `missing → <KEY>`.

## Aspects

- [js-execution](../aspects/js-execution.md): limits, timeouts and memory of the VM that runs `handle()`.
- [js-registry](../aspects/js-registry.md): the prepared script per hook behind `jsrun.Runner`, and its rebuild.
- [js-sources](../aspects/js-sources.md): where the hook source comes from.
- [kube-access](../aspects/kube-access.md): how the dispatcher and hooks reach the cluster.
- [status-conditions](../aspects/status-conditions.md): conditions and events a failing or restarted hook shows.
- [api-design](../aspects/api-design.md): the rule that a field needs an implementation (API-1).

## Decisions

- **One subscription per hook, with one informer per kubernetes binding, one rate-limited FIFO workqueue and one worker goroutine.** Status: accepted (commit history, no date or name recorded). Why: calls must not overlap and the VM rescue after a call is owned by one goroutine. Not taken: a worker per binding, because it breaks R9; reasons for others are not recorded.
- **The queue holds only `eventKey` struct values; payloads sit in a pending map.** Status: accepted (commit history). Why: BindingContext is not hashable, equal keys fold bursts (R8).
- **`Subscribe` waits for the old worker before it starts the new one, under a per-hook lock; the dispatcher lock only guards the subscription map.** Status: accepted. Why: a slow cache sync of one hook must not block other hooks (R17); the old worker must still be gone before the new one starts (R9).
- **Subscribe only after a VM (re)start or a generation change; a rebuild of the same source (limits changed, manual restart) does not re-subscribe.** Status: accepted (commit history). Why: the worker looks up the prepared script per call, and the same source gives the same `config()`.
- **Failed calls are requeued with the default controller rate limiter and no retry cap.** Status: accepted (commit history). Known debt: DISP-10.
- **A hook's bindings live in `spec.bindings`, not in a `config()` the script returns.** Status: accepted, replaces the earlier `config()` decision (api-design.R11). Why: the operator has to know a hook's scope before it runs the hook's code; deriving RBAC from bindings is impossible otherwise. Not taken: shell-operator's `config()`, because its dynamism never existed here — it ran once per build.
- **A namespace is selected by `namespaceSelector`, not by a list of names.** Status: accepted. Why: one selector covers one namespace (`kubernetes.io/metadata.name`), many namespaces and label-based sets, and it is the field `JSAdmission` already has (api-design.R10). Not taken: a `namespaces` list next to the selector, because two ways to say the same thing drift.
- **Wildcards are rejected in a hook binding.** Status: accepted. Why: an informer watches one concrete resource; `*` would have to be expanded against discovery and re-expanded whenever a CRD is installed (R21). Not taken: expanding `*` at subscribe time, because the hook would silently miss resources added later.

## Open

DISP-4, DISP-5, DISP-6, DISP-10, DISP-12, DISP-13, API-1, EXEC-2
