// Package dispatcher subscribes the controller to the Kubernetes events that
// each JSHook's config() declared, and forwards them as BindingContext payloads
// to that hook's persistent JS instance.
//
// One Dispatcher serves all JSHooks. Per JSHook it owns:
//   - a context (so Drop can cancel the goroutines deterministically),
//   - a per-hook FIFO queue (serializes handle() calls — required by the
//     persistent-instance contract: top-level state isn't safe under parallel
//     access from inside the same QuickJS runtime),
//   - one watcher goroutine per kubernetes binding,
//   - one worker goroutine that drains the queue.
//
// The dispatcher is intentionally separate from controller-runtime's shared
// cache because each binding can carry its own label/field selectors and
// namespace filter that don't compose cleanly with a single shared informer.
package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jshook"
	"github.com/o-haase/gojsop/internal/jslifecycle"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// EventEmitter is the shared lifecycle-event callback. Aliased from
// jslifecycle so existing callers (dispatcher.EventEmitter) keep working
// while the type lives in one place.
type EventEmitter = jslifecycle.EventEmitter

// eventKey identifies a queued BindingContext. All fields are strings, so the
// struct is hashable and can be used directly as a workqueue key — the queue
// dedupes by struct equality, collapsing bursts on the same object into one
// entry while still distinguishing Added/Modified/Deleted and old/new UIDs.
//
// For Synchronization-type contexts only `binding` and `event` are populated.
// jshook.R8
type eventKey struct {
	binding   string
	event     string
	namespace string
	name      string
	uid       string
}

// Dispatcher routes Kubernetes events into per-hook JS handlers.
type Dispatcher struct {
	dyn    dynamic.Interface
	mapper RESTMapper
	reg    jsrun.Runner

	mu       sync.Mutex // guards subs and keyLocks only; never held across slow work
	subs     map[types.NamespacedName]*subscription
	keyLocks map[types.NamespacedName]*sync.Mutex
}

// RESTMapper is the slice of controller-runtime's mapper interface we need —
// resolving apiVersion/kind into a GroupVersionResource for the dynamic client.
type RESTMapper interface {
	RESTMapping(gk schema.GroupKind, versions ...string) (*RESTMapping, error)
}

// RESTMapping mirrors meta.RESTMapping minus the parts we don't use here,
// avoiding a hard dep on apimachinery/meta in tests.
type RESTMapping struct {
	Resource schema.GroupVersionResource
}

// New creates an empty Dispatcher. Pass the cluster's dynamic client, a
// RESTMapper (controller-runtime's mgr.GetRESTMapper() can be wrapped to fit),
// and the Runner that owns per-hook persistent JS instances. The runner
// is required so that worker rescue paths (memory/panic/timeout) can rebuild
// the instance and the next call sees the new one transparently.
func New(dyn dynamic.Interface, mapper RESTMapper, reg jsrun.Runner) *Dispatcher {
	return &Dispatcher{
		dyn:      dyn,
		mapper:   mapper,
		reg:      reg,
		subs:     make(map[types.NamespacedName]*subscription),
		keyLocks: make(map[types.NamespacedName]*sync.Mutex),
	}
}

// Subscribe (re)wires informers for hook `key`. The worker resolves the live
// instance via the Runner on every dispatch. cfg drives which informers start.
// emit (optional) publishes lifecycle events; pass nil in tests.
//
// If a subscription already exists for key, it is torn down first.
//
// d.mu only guards the subs map. The slow parts (stopping the old worker,
// waiting for informer sync) run under the per-key lock, so one hook that
// cannot sync never blocks Subscribe or Drop of another hook. Drop of the
// same key cancels a Subscribe that is still waiting; that Subscribe then
// returns an error.
// jshook.R17
func (d *Dispatcher) Subscribe(parent context.Context, key types.NamespacedName, cfg *jshook.Config, emit EventEmitter) error {
	if d.reg == nil {
		return fmt.Errorf("dispatcher: Runner is nil — Subscribe needs the runner to resolve live instances")
	}

	kl := d.keyLock(key)
	kl.Lock()
	defer kl.Unlock()

	// Old worker must be gone before the new one starts (jshook.R9).
	d.mu.Lock()
	old := d.subs[key]
	delete(d.subs, key)
	d.mu.Unlock()
	if old != nil {
		old.stop()
	}

	ctx, cancel := context.WithCancel(parent)
	sub := &subscription{
		key: key,
		reg: d.reg,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[eventKey](),
			workqueue.TypedRateLimitingQueueConfig[eventKey]{Name: key.String()},
		),
		cancel:  cancel,
		pending: make(map[eventKey]jshook.BindingContext),
		emit:    emit,
	}
	// Registered before the watchers start so Drop can cancel a sync that hangs.
	d.mu.Lock()
	d.subs[key] = sub
	d.mu.Unlock()

	fail := func(err error) error {
		sub.stop()
		d.mu.Lock()
		if d.subs[key] == sub {
			delete(d.subs, key)
		}
		d.mu.Unlock()
		return err
	}

	for _, b := range cfg.Kubernetes {
		gv, err := schema.ParseGroupVersion(b.APIVersion)
		if err != nil {
			return fail(fmt.Errorf("binding %q: parse apiVersion %q: %w", b.Name, b.APIVersion, err))
		}
		mapping, err := d.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: b.Kind}, gv.Version)
		if err != nil {
			return fail(fmt.Errorf("binding %q: REST mapping for %s/%s: %w", b.Name, b.APIVersion, b.Kind, err))
		}
		if err := sub.startWatcher(ctx, d.dyn, mapping.Resource, b); err != nil {
			return fail(fmt.Errorf("binding %q: start watcher: %w", b.Name, err))
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("subscribe canceled: %w", err))
	}

	sub.wg.Add(1)
	go sub.runWorker(ctx)
	return nil
}

// keyLock returns the lock that serializes Subscribe and Drop of one hook.
func (d *Dispatcher) keyLock(key types.NamespacedName) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	kl, ok := d.keyLocks[key]
	if !ok {
		kl = &sync.Mutex{}
		d.keyLocks[key] = kl
	}
	return kl
}

// Drop tears down all subscriptions for the given key. A Subscribe of the
// same key that still waits for informer sync is cancelled first, so Drop
// does not wait for the sync.
// jshook.R14
func (d *Dispatcher) Drop(key types.NamespacedName) {
	d.mu.Lock()
	sub := d.subs[key]
	d.mu.Unlock()
	if sub != nil {
		sub.cancel()
	}

	kl := d.keyLock(key)
	kl.Lock()
	defer kl.Unlock()
	d.mu.Lock()
	sub = d.subs[key]
	delete(d.subs, key)
	d.mu.Unlock()
	if sub != nil {
		sub.stop()
	}
}

// subscription is one JSHook's slice of the world.
//
// The workqueue holds eventKey structs; the associated BindingContext payload
// lives in `pending`. The split exists because BindingContext contains
// map/slice fields and is therefore not hashable, so it cannot itself be a
// workqueue item. Keying on object identity also collapses bursts of Modified
// events for the same resource into one queue entry that always carries the
// freshest snapshot — which is what hook authors expect.
type subscription struct {
	key    types.NamespacedName
	reg    jsrun.Runner
	queue  workqueue.TypedRateLimitingInterface[eventKey]
	cancel context.CancelFunc
	wg     sync.WaitGroup

	pendMu  sync.Mutex
	pending map[eventKey]jshook.BindingContext

	// emit publishes corev1.Events about this subscription's hook. Nil when
	// the reconciler did not configure a recorder (test default).
	emit EventEmitter
}

// stop signals the worker and watchers to wind down, then blocks until the
// worker goroutine has actually exited. Subscribe() relies on this so that
// re-subscribing the same hook can never overlap an in-flight handle() call
// from the previous subscription.
// jshook.R9
func (s *subscription) stop() {
	s.queue.ShutDown()
	s.cancel()
	s.wg.Wait()
}

// jshook.R3
func (s *subscription) startWatcher(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, b jshook.KubernetesBinding) error {
	ns := metav1.NamespaceAll
	if b.Namespace != nil && b.Namespace.NameSelector != nil && len(b.Namespace.NameSelector.MatchNames) == 1 {
		// MVP: single-namespace fast path. Multi-namespace fan-out comes later.
		ns = b.Namespace.NameSelector.MatchNames[0]
	}
	sel := labels.Everything()
	if len(b.LabelSelector) > 0 {
		// Best-effort: if labelSelector has matchLabels: {k:v}, encode it.
		if ml, ok := b.LabelSelector["matchLabels"].(map[string]any); ok {
			set := labels.Set{}
			for k, v := range ml {
				if vs, ok := v.(string); ok {
					set[k] = vs
				}
			}
			sel = set.AsSelector()
		}
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, ns, func(o *metav1.ListOptions) {
		o.LabelSelector = sel.String()
	})
	informer := factory.ForResource(gvr).Informer()

	wantedEvents := map[string]bool{}
	if len(b.ExecuteHookOnEvent) == 0 {
		wantedEvents = map[string]bool{"Added": true, "Modified": true, "Deleted": true}
	} else {
		for _, e := range b.ExecuteHookOnEvent {
			wantedEvents[e] = true
		}
	}

	bindingName := b.Name
	if bindingName == "" {
		bindingName = b.Kind
	}

	enqueueEvent := func(eventName string, raw map[string]any) {
		if !wantedEvents[eventName] {
			return
		}
		md, _ := raw["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		name, _ := md["name"].(string)
		uid, _ := md["uid"].(string)
		k := eventKey{binding: bindingName, event: eventName, namespace: ns, name: name, uid: uid}
		bc := jshook.BindingContext{
			Binding:    bindingName,
			Type:       "Event",
			WatchEvent: eventName,
			Object:     raw,
		}
		s.pendMu.Lock()
		s.pending[k] = bc
		s.pendMu.Unlock()
		s.queue.Add(k)
	}

	// gate buffers every event handler call until Synchronization is published.
	// We never drop pre-sync events: between WaitForCacheSync and the gate flip
	// a Modified or Deleted may legitimately arrive for an object that's about
	// to ship in the snapshot, and silently dropping it would lose an update.
	// After the gate opens, the buffer is drained — Added redeliveries for
	// objects already in the snapshot are filtered out via initialUIDs to keep
	// shell-operator semantics (Synchronization first, then strict deltas).
	type bufferedEvent struct {
		eventName string
		raw       map[string]any
	}
	var (
		gateMu   sync.Mutex
		gateOpen bool
		preSync  []bufferedEvent
	)
	gated := func(eventName string, obj any) {
		raw := toRaw(obj)
		if raw == nil {
			return
		}
		gateMu.Lock()
		if !gateOpen {
			preSync = append(preSync, bufferedEvent{eventName, raw})
			gateMu.Unlock()
			return
		}
		gateMu.Unlock()
		enqueueEvent(eventName, raw)
	}

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { gated("Added", o) },
		UpdateFunc: func(_, n any) { gated("Modified", n) },
		DeleteFunc: func(o any) { gated("Deleted", o) },
	})
	if err != nil {
		return err
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("binding %q: cache sync canceled", bindingName)
	}

	syncEnabled := b.ExecuteHookOnSynchronization == nil || *b.ExecuteHookOnSynchronization

	gateMu.Lock()
	objs := informer.GetStore().List()
	syncObjects := make([]jshook.SyncObject, 0, len(objs))
	initialUIDs := make(map[string]bool, len(objs))
	for _, o := range objs {
		raw := toRaw(o)
		if raw == nil {
			continue
		}
		if uid := uidOf(raw); uid != "" {
			initialUIDs[uid] = true
		}
		syncObjects = append(syncObjects, jshook.SyncObject{Object: raw})
	}
	if syncEnabled {
		s.enqueueSynchronization(bindingName, syncObjects)
	} else {
		// Sync disabled: replay initial state as Added events so hooks that
		// opted out of Synchronization still see what existed at startup.
		for _, so := range syncObjects {
			enqueueEvent("Added", so.Object)
		}
		// initialUIDs is no longer needed for dedupe — pre-sync Added events
		// in the buffer for those objects are redeliveries of what we just
		// replayed, but the workqueue's struct-key dedupe collapses them
		// into one entry already.
		initialUIDs = nil
	}
	buffered := preSync
	preSync = nil
	gateOpen = true
	gateMu.Unlock()

	// Drain pre-sync buffer in arrival order. Added events for objects that
	// already shipped in the Synchronization snapshot are dropped — those are
	// redeliveries from client-go's sharedProcessor notification buffer (it
	// holds pending notifications for a brief window after WaitForCacheSync
	// returns). Modified/Deleted events flow through unconditionally; they
	// represent state changes the snapshot can't capture.
	for _, ev := range buffered {
		if ev.eventName == "Added" && initialUIDs != nil {
			if uid := uidOf(ev.raw); uid != "" && initialUIDs[uid] {
				delete(initialUIDs, uid)
				continue
			}
		}
		enqueueEvent(ev.eventName, ev.raw)
	}
	return nil
}

// toRaw extracts the map[string]any payload from an informer-emitted object.
// Dynamic informers ship *unstructured.Unstructured; the worker only ever
// deals with the underlying map.
func toRaw(obj any) map[string]any {
	if raw, ok := obj.(map[string]any); ok {
		return raw
	}
	if u, ok := obj.(interface{ UnstructuredContent() map[string]any }); ok {
		return u.UnstructuredContent()
	}
	return nil
}

func uidOf(raw map[string]any) string {
	md, _ := raw["metadata"].(map[string]any)
	uid, _ := md["uid"].(string)
	return uid
}

// enqueueSynchronization ships a single Synchronization-type BindingContext
// carrying the snapshot of existing objects. shell-operator parity: hooks see
// this once per (re)Subscribe before any per-object events.
// jshook.R3
func (s *subscription) enqueueSynchronization(bindingName string, objs []jshook.SyncObject) {
	k := eventKey{binding: bindingName, event: "Synchronization"}
	bc := jshook.BindingContext{
		Binding: bindingName,
		Type:    "Synchronization",
		Objects: objs,
	}
	s.pendMu.Lock()
	s.pending[k] = bc
	s.pendMu.Unlock()
	s.queue.Add(k)
}

func (s *subscription) runWorker(ctx context.Context) {
	defer s.wg.Done()
	logger := log.FromContext(ctx).WithValues("hook", s.key.String())
	for {
		qkey, shutdown := s.queue.Get()
		if shutdown {
			return
		}
		s.pendMu.Lock()
		bc, present := s.pending[qkey]
		delete(s.pending, qkey)
		s.pendMu.Unlock()
		if !present {
			s.queue.Done(qkey)
			continue
		}
		s.handleEvent(ctx, logger, qkey, bc)
		s.queue.Done(qkey)
	}
}

// handleEvent runs one BindingContext through the live VM and dispatches on
// the outcome. The locking + recover + OOM/cancel classification lives in
// jsrun.Runner.Invoke; the dispatcher only owns the policy table:
//
//   - panic / OOM / cancelled (timeout) → rescue + requeue
//   - other error → log + emit Warning + requeue
//   - ok → forget
//
// Cancellation: the wazero deadline (carried by callCtx) bounds wall-clock at
// the spec'd budget and surfaces as OutcomeCancelled. wazero closes the module
// when the context ends, so the VM is dead afterwards and is rescued at once.
// jshook.R10
// jshook.R11
// jshook.R12
func (s *subscription) handleEvent(parent context.Context, logger logr.Logger, qkey eventKey, bc jshook.BindingContext) {
	inst, ok := s.reg.Instance(jsrun.HookKey(s.key))
	if !ok {
		s.noVM(logger, qkey, bc)
		return
	}

	budget := time.Duration(inst.Limits.TimeoutSeconds) * time.Second
	callCtx, cancel := contextWithOptionalTimeout(parent, budget)
	defer cancel()

	out, res, err := jshook.Handle(callCtx, s.reg, jsrun.HookKey(s.key), []jshook.BindingContext{bc})
	if err != nil {
		// ErrVMUnavailable (rescued meanwhile) retries; ErrUnknownKey: the VM
		// was dropped between Get and Call (race with reconciler delete).
		if errors.Is(err, jsrun.ErrVMUnavailable) {
			s.noVM(logger, qkey, bc)
			return
		}
		logger.Info("hook vanished mid-call", "binding", bc.Binding, "event", bc.WatchEvent)
		s.queue.Forget(qkey)
		return
	}

	switch res.Outcome {
	case jsrun.OutcomePanic:
		logger.Error(fmt.Errorf("panic in handle(): %v", res.Panic),
			"handle() panicked — restarting instance",
			"binding", bc.Binding, "event", bc.WatchEvent, "stack", string(debug.Stack()))
		s.rescue(logger, jsrun.ReasonPanic)
		s.requeue(qkey, bc)

	case jsrun.OutcomeMemoryLimit:
		logger.Error(res.Err, "handle() hit memory limit — restarting instance",
			"binding", bc.Binding, "event", bc.WatchEvent)
		s.rescue(logger, jsrun.ReasonMemoryLimit)
		s.requeue(qkey, bc)

	case jsrun.OutcomeCancelled:
		// The call's context ended and wazero closed the module, so this VM
		// is dead whatever ended the call: rebuild it at once. Only the
		// per-call deadline is a timeout of the hook and earns a Warning; a
		// cancelled parent means Subscribe or Drop is stopping this worker.
		if parent.Err() == nil {
			s.publish(corev1.EventTypeWarning, conditions.EventHandleTimeout,
				fmt.Sprintf("handle() exceeded %s", budget))
		}
		logger.Info("handle() cancelled — restarting instance",
			"binding", bc.Binding, "event", bc.WatchEvent, "elapsed", res.Duration)
		s.rescue(logger, jsrun.ReasonTimeout)
		s.requeue(qkey, bc)

	case jsrun.OutcomeError:
		logger.Error(res.Err, "handle() failed", "binding", bc.Binding, "event", bc.WatchEvent)
		// Static message — the JS error string would explode dedup
		// cardinality at admission/event-loop volume. The verbose error
		// goes to logs and to status.lastExecution.error.
		s.publish(corev1.EventTypeWarning, conditions.EventHandleFailed,
			"handle() returned an error")
		s.requeue(qkey, bc)

	default: // OutcomeOK
		if out != "" {
			logger.V(1).Info("handle() returned", "value", out)
		}
		s.queue.Forget(qkey)
	}
}

// noVM handles an event for a hook that has no VM now. A hook that is
// Building or Broken keeps its events: they are requeued with the rate limiter
// and meet the new VM. A hook the registry no longer knows loses them.
// jshook.R19
func (s *subscription) noVM(logger logr.Logger, qkey eventKey, bc jshook.BindingContext) {
	if !s.reg.Known(jsrun.HookKey(s.key)) {
		logger.Error(nil, "hook not in registry — dropping event",
			"binding", bc.Binding, "event", bc.WatchEvent)
		s.queue.Forget(qkey)
		return
	}
	logger.V(1).Info("hook has no VM yet — requeueing event",
		"binding", bc.Binding, "event", bc.WatchEvent)
	s.requeue(qkey, bc)
}

// contextWithOptionalTimeout returns a derived context with the given
// timeout, or the parent unchanged (with a no-op cancel) when timeout <= 0.
func contextWithOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

// rescue closes the instance and starts its rebuild via jslifecycle.Rescue
// (which publishes the canonical Restarted / RescueFailed events). It does not
// wait for the build: until it ends calls find no VM and the event is requeued
// with backoff (handleEvent); a failed build shows as BuildFailed on the hook.
// jshook.R11
// jshook.R12
func (s *subscription) rescue(logger logr.Logger, reason jsrun.RestartReason) {
	if err := jslifecycle.Rescue(s.reg, jsrun.HookKey(s.key), reason, s.emit); err != nil {
		logger.Error(err, "rescue restart failed", "reason", reason)
	}
}

// publish forwards to the EventEmitter the reconciler installed at
// Subscribe time. Nil-safe.
func (s *subscription) publish(eventType, reason, message string) {
	if s.emit != nil {
		s.emit(eventType, reason, message)
	}
}

// requeue stashes bc back under qkey (unless a fresher event arrived) and
// re-enqueues with rate-limited backoff.
// jshook.R13
func (s *subscription) requeue(qkey eventKey, bc jshook.BindingContext) {
	s.pendMu.Lock()
	if _, fresher := s.pending[qkey]; !fresher {
		s.pending[qkey] = bc
	}
	s.pendMu.Unlock()
	s.queue.AddRateLimited(qkey)
}
