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

This area describes what an engineer who writes a JSHook can rely on: how the hook says what it
watches, and when gojsop calls its `handle()` with what.

A hook is one `JSHook` resource. `spec.bindings` says what it watches;
`spec.source` holds a script that defines `handle(contexts)`. The author
writes JavaScript, applies the `JSHook` and reads its status. They run no
controller.

A binding names resources like a webhook rule (apiGroups, apiVersions,
resources) and narrows them with `namespaceSelector` and `objectSelector`,
the same shape as in `JSAdmission` (api-design.R10).

Per binding, `handle()` first gets one `Synchronization` with every matching
object, then one `Event` per change (`Added`, `Modified`, `Deleted`).

Words used here:

- **script**: the hook's source, loaded and prepared by gojsop. A new script
  is prepared when the source, the limits or the restart annotation change.
- **call**: one run of `handle()`. Every call starts from the freshly
  prepared script; nothing a call leaves behind reaches the next one.

## Use cases

### jshook.UC1 Declare what a hook watches

- **Actor**: hook author
- **Trigger**: applies a `JSHook` with `spec.bindings` and a source that defines `handle()`
- **Before**: the source loads ([js-sources](../aspects/js-sources.md))
- **Steps**:
  1. The author lists bindings: `name`, `apiGroups`, `apiVersions`, `resources`, optional selectors.
  2. gojsop checks every binding against the cluster, then starts one watch per resource.
  3. `status.bindings` lists what is watched.
- **Exceptions**: `*`, a resource the cluster does not serve, or two bindings on one resource fail the reconcile; the hook keeps the watches it had (jshook.R20, jshook.R21). A script without `handle()` fails (jshook.R2).
- **Result**: the hook watches exactly what it declared.

### jshook.UC2 Receive the current state, then changes

- **Actor**: hook author
- **Trigger**: a binding starts watching (hook created, bindings or source changed)
- **Before**: UC1
- **Steps**:
  1. `handle()` gets one `Synchronization` with all matching objects.
  2. Then it gets one `Event` per change.
- **Exceptions**: with `synchronization: false` existing objects arrive as `Added` (jshook.R5). Changes during the snapshot are not lost (jshook.R3, jshook.R4).
- **Result**: the hook sees the start state, then only the changes.

### jshook.UC3 Choose which events and objects reach the hook

- **Actor**: hook author
- **Trigger**: sets `events` and selectors in a binding
- **Before**: UC1
- **Steps**:
  1. The author lists event types, or leaves `events` out to get all three.
  2. The author narrows objects with `objectSelector` and namespaces with `namespaceSelector`. One namespace is selected by `kubernetes.io/metadata.name`.
- **Exceptions**: a `namespaceSelector` never matches a cluster-scoped object (jshook.R22).
- **Result**: `handle()` runs only for what the author asked for.

### jshook.UC4 Handle a burst of changes

- **Actor**: hook author
- **Trigger**: one object changes faster than `handle()` runs
- **Before**: UC2
- **Steps**: waiting changes of that object fold into one call with the newest object.
- **Exceptions**: changes of different objects stay separate calls.
- **Result**: the hook gets no stale state, and its calls never overlap (jshook.R8, jshook.R9).

### jshook.UC5 Recover from a failing call

- **Actor**: hook author
- **Trigger**: `handle()` throws, runs past `timeoutSeconds`, hits the memory limit or crashes the engine
- **Before**: the hook runs
- **Steps**:
  1. gojsop stops the call and records a Warning event on the `JSHook`.
  2. The event is retried with backoff. The retry starts from the prepared script; nothing is prepared again.
- **Exceptions**: a hook that always fails is retried forever (DISP-10).
- **Result**: one bad call costs only that call (jshook.R10 to jshook.R13).

### jshook.UC6 Remove a hook

- **Actor**: hook author
- **Trigger**: deletes the `JSHook`
- **Result**: `handle()` is not called again (jshook.R14).

### jshook.UC7 Change the source

- **Actor**: hook author
- **Trigger**: changes the source, the limits or the restart annotation
- **Before**: the hook runs
- **Steps**:
  1. gojsop prepares the new script in the background; the hook shows `Ready=False`, reason `Building`.
  2. Events wait and reach the new script once it is ready.
- **Exceptions**: a script that fails to prepare shows `BuildFailed` and is retried with backoff; a new source is tried at once (jshook.R18, jshook.R19, jshook.R24).
- **Result**: the hook runs the new script, and no event is lost.

## Rules

| ID | Rule | Source | Held by |
|---|---|---|---|
| jshook.R1 | `spec.bindings` alone decides what a hook watches. Changing it re-subscribes the hook without preparing the script again. | api-design.R11 | `TestSummarizeBindings_ListsEveryWatchedResource`, `TestControllers` |
| jshook.R2 | A script without `handle()` fails to prepare and says so. | `requireHandle` | `TestRequireHandle_PostBuildRejectsMissingHandle`, `TestRequireHandle_AcceptsHandleOnly`, `TestControllers` |
| jshook.R3 | The `Synchronization` comes first; changes during it follow and are not dropped. | shell-operator parity | `TestDispatcher_SynchronizationFirstThenDeltas` |
| jshook.R4 | An object already in the `Synchronization` does not arrive again as `Added`. | shell-operator parity | `TestDispatcher_SynchronizationFirstThenDeltas` |
| jshook.R5 | With `synchronization: false` there is no `Synchronization`; existing objects arrive as `Added`. | `HookBinding.WantsSynchronization` | `TestDispatcher_SyncDisabled_ExistingObjectsArriveAsAdded` |
| jshook.R6 | Only event types listed in `events` arrive; an empty list means all three. | `HookBinding.WantsEvent` | `TestDispatcher_ExecuteHookOnEvent_ListedTypesOnly`, `TestDispatcher_ExecuteHookOnEvent_EmptyMeansAll` |
| jshook.R7 | `handle()` gets a one-element array. An `Event` has `binding`, `type`, `watchEvent`, `object`; a `Synchronization` has `binding`, `type`, `objects`. | `jshook.BindingContext` | `TestHandle_ReceivesBindingContext` |
| jshook.R8 | Waiting changes of one object fold into one call with the newest object. | shell-operator parity | `TestDispatcher_BurstOfChangesFoldsIntoNewestObject` |
| jshook.R9 | Two calls of one hook never run at the same time, also not while its bindings change. | decision "one worker per hook" | `TestDispatcher_ResubscribeDuringCall_NoOverlapAndProcessAlive` |
| jshook.R10 | A throwing `handle()` records a Warning event, and the event is retried with backoff. | decision "retry without cap" | `TestDispatcher_ThrowingHandle_WarnsRetriesWithoutRestart` |
| jshook.R11 | A call that runs past `timeoutSeconds` is stopped, records a Warning event and is retried. | js-execution.R3 | `TestDispatcher_Timeout_CancelsWarnsAndKeepsVM` |
| jshook.R12 | A call that hits the memory limit or crashes the engine is retried. The memory limit records a Warning event. | js-registry.R2 | `TestDispatcher_MemoryLimit_WarnsKeepsVMAndRetries`, `TestDispatcher_PanicInHandle_RetriesOnFreshVM` |
| jshook.R13 | A retry never overwrites a newer state of the same object. | decision "retry without cap" | `TestDispatcher_RetryKeepsFresherStateOfSameObject` |
| jshook.R14 | A deleted hook gets no more calls. | `Dispatcher.Drop` | `TestDispatcher_Drop_NoMoreEvents` |
| jshook.R15 | `objectSelector` and `namespaceSelector` filter objects in every `LabelSelector` form, `matchExpressions` included. | `corev1alpha1.ObjectMatch` | `TestControllers` |
| jshook.R16 | Every field of a binding takes effect; none is accepted and then ignored. | api-design.R6 | `TestSummarizeBindings_ListsEveryWatchedResource` |
| jshook.R17 | A hook whose watch cannot start does not delay other hooks; deleting it ends the wait. | decision "per-hook lock" | `TestDispatcher_SlowSync_DoesNotBlockOtherHooks` |
| jshook.R18 | While a new script is prepared the hook is `Ready=False`, reason `Building`. | status-conditions.R7 | `TestReconcile_HangingBuildDoesNotBlockOtherHook` |
| jshook.R19 | Events that arrive while no script is ready wait and reach the script once it is ready; none is dropped. | js-registry.R19 | `TestDispatcher_NoVM_KeepsEventsAndDeliversAfterBuild` |
| jshook.R20 | A binding that cannot be resolved fails the reconcile, and the hook keeps the watches it had. | decision "reject before touching watches" | `TestPlanWatches_BadBindingKeepsExistingWatches` |
| jshook.R21 | A binding names a concrete apiGroup, apiVersion and resource. `*`, a resource the cluster does not serve and a resource two bindings share are rejected. | decision "no wildcards", api-design.R10 | `TestPlanWatches_RejectsWildcardUnknownAndDuplicate` |
| jshook.R22 | `namespaceSelector` matches the labels of the object's namespace. A cluster-scoped object matches only when there is no `namespaceSelector`. | Kubernetes webhook semantics | `TestControllers` |
| jshook.R23 | `status.bindings` has one entry `name:group/version/resource` per watched resource, and each entry is a running watch. | `summarizeBindings` | `TestSummarizeBindings_ListsEveryWatchedResource` |
| jshook.R24 | A script that fails to prepare shows `BuildFailed` and is retried with backoff; a new source is tried at once. | status-conditions.R7, js-registry.R17 | `TestReconcile_BrokenBuild_BacksOffAndSourceChangeRebuildsAtOnce` |
| jshook.R25 | A successful call ends the retries of its event. | decision "retry without cap" | `TestDispatcher_ThrowingHandle_WarnsRetriesWithoutRestart` |

Every rule is held by a test or is `missing → <KEY>`.

## Aspects

- [js-execution](../aspects/js-execution.md): limits, timeouts and memory of a call.
- [js-registry](../aspects/js-registry.md): how a script is prepared and called.
- [js-sources](../aspects/js-sources.md): where the source comes from.
- [kube-access](../aspects/kube-access.md): `kube.*` and RBAC.
- [status-conditions](../aspects/status-conditions.md): conditions and events of a hook.
- [api-design](../aspects/api-design.md): every field needs an implementation (api-design.R6).

## Decisions

- **Bindings live in `spec.bindings`, not in a `config()` the script returns.** Status: accepted (api-design.R11). Why: gojsop must know a hook's scope, and derive its RBAC, before it runs the hook's code. Not taken: shell-operator's `config()`.
- **One worker per hook: one queue, one goroutine for all its bindings.** Status: accepted. Why: calls must not overlap (R9). Not taken: a worker per binding, which breaks R9.
- **The queue holds object keys; the payload waits beside it.** Status: accepted. Why: equal keys fold bursts (R8).
- **Re-subscribe under a per-hook lock, after the old worker stopped.** Status: accepted. Why: a slow hook must not block others (R17), and two workers must never overlap (R9).
- **Re-subscribe only when the source or the generation changed.** Status: accepted. Why: a script prepared again from the same source watches the same bindings.
- **Retry failed calls with the default controller rate limiter, without a cap.** Status: accepted. Known debt: DISP-10.
- **Select namespaces by `namespaceSelector`, not by a list of names.** Status: accepted. Why: one selector covers one, many or labelled namespaces, and `JSAdmission` has the same field (api-design.R10). Not taken: a `namespaces` list, because two ways to say one thing drift.
- **Reject wildcards in a binding.** Status: accepted. Why: a watch is on one concrete resource; an expanded `*` would silently miss resources installed later (R21).

## Open

DISP-4, DISP-5, DISP-6, DISP-10, DISP-13, API-1, EXEC-2
