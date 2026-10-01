---
id: hook-dispatch
status: accepted
entrypoints:
  - jshook.ReadConfig
  - jshook.Handle
  - dispatcher.Dispatcher.Subscribe
  - dispatcher.Dispatcher.Drop
---

# Hook Dispatch

This block describes how a JSHook declares what it wants to watch and how
gojsop turns Kubernetes changes into `handle()` calls.

gojsop follows the shell-operator model. A hook exports two functions.
`config()` returns a list of bindings: what the hook wants to react to. A
binding of kind `kubernetes` names a resource and optional filters. The
controller reads `config()` once each time the VM is built. `handle(contexts)`
does the work. It receives one binding context per call, a plain object that
says which binding fired and what changed.

A binding produces two kinds of context. First comes one `Synchronization`
context: a snapshot with all matching objects that exist when the watch
starts. After that come `Event` contexts, one per change (`Added`, `Modified`,
`Deleted`) with the object and the watch event. The hook can turn the snapshot
off with `executeHookOnSynchronization: false`. Then each existing object is
delivered as an `Added` event instead.

The dispatcher keeps one subscription per hook. A subscription has one
informer per kubernetes binding, one rate-limited workqueue and one worker
goroutine. The worker takes the next item and calls `handle()` through
`jsregistry.Registry.Call` (see [js-registry](js-registry.md)). Calls for one
hook never overlap. The queue is keyed by binding, event, namespace, name and
UID. Bursts of changes to one object collapse into one call with the newest
object. Events that arrive before the snapshot are buffered, not dropped.

A hook that times out three times in a row is rescued: its VM is rebuilt (see
[js-registry](js-registry.md)). A failed call is retried with the default
controller rate limiter and no retry cap.

Only `kubernetes` bindings run. `schedule`, `onStartup`, `jqFilter`,
`allowFailure` and `queue` are decoded but have no effect. Selectors are
applied in part: one namespace from `namespace.nameSelector.matchNames` and
`labelSelector.matchLabels` with string values. The reason for these limits is
not recorded; the code marks the namespace handling as an MVP.

Why one subscription with one worker, why struct keys and why Synchronization
comes first are recorded only as commit messages: shell-operator parity, no
lost pre-snapshot events, and no overlap on re-subscribe. Rejected
alternatives are not recorded.

## Rules

- **R1** Read `config()` through `jshook.ReadConfig` once per VM build, and
  fail the build when the export `config` or `handle` is missing.
  Why: the hook is checked when it is built, not when the first event arrives.
  Gate: `TestReadConfig_FromHookSource`, `TestReadConfig_MissingFunction`.
- **R2** Keep one subscription and one worker per hook, and let `Subscribe`
  wait for the old worker before it starts the new one.
  Why: `handle()` calls must not overlap, and worker state such as the timeout
  streak relies on a single goroutine.
  Gate: missing → GATE-17.
- **R3** Queue only `eventKey` values and keep the payload in the pending map.
  Why: equal keys collapse bursts, and the call sees the newest object.
  Gate: missing → GATE-17.
- **R4** Deliver the Synchronization context first, buffer earlier events and
  drop `Added` events for UIDs already in the snapshot.
  Why: shell-operator parity, and no event is lost before the snapshot.
  Gate: missing → GATE-17.
- **R5** Call the VM from the dispatcher only through `Registry.Call`, one
  context per call as a one-element array, with a timeout from the VM limits.
  Why: lock, panic recovery and result classification live in the registry.
  Gate: `TestHandle_ReceivesBindingContext`, `TestHandle_MissingFunction`.
- **R6** Rescue a hook after three consecutive timeouts, and after a panic or
  a memory-limit error. Reset the streak on success or on a plain error.
  Why: a hook that keeps timing out is assumed to hold a broken VM. No further
  reason is recorded.
  Gate: missing → GATE-17.
- **R7** Requeue a failed call with rate limiting, keep the old payload only
  if no fresher one is pending, and `Forget` the key on success.
  Why: a failed call must be retried without overwriting newer state.
  Gate: missing → GATE-17.
- **R8** Subscribe only after a VM (re)start or a generation change, and drop
  the subscription before the VM when the hook is deleted.
  Why: the worker resolves the live VM per call, so a rescue needs no
  re-subscribe, but a deleted hook must stop receiving events.
  Gate: missing → GATE-17.
- **R9** Do not advertise a binding field that has no reader.
  Why: project rule API-1.
  Gate: missing → GATE-17. Violated today → DISP-1, DISP-2, DISP-3, DISP-11.

## Open

Tracked in [backlog](../backlog.md): DISP-1 to DISP-11, STAT-5, API-1, REG-4,
EXEC-2, DOC-4; gates GATE-17.
