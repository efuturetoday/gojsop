// Package dispatcher subscribes the controller to the Kubernetes events that
// each JSHook's spec.bindings declared, and forwards them as BindingContext payloads
// to a fresh instance of that hook's prepared script.
//
// One Dispatcher serves all JSHooks. Per JSHook it owns:
//   - a context (so Drop can cancel the goroutines deterministically),
//   - a per-hook FIFO queue (serializes handle() calls so a burst on one
//     object folds into the newest view instead of racing),
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

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jshook"
	"github.com/o-haase/gojsop/internal/jslifecycle"
	"github.com/o-haase/gojsop/internal/jslog"
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

	// nsStore caches namespaces for namespaceSelector, shared by every hook
	// and started on first use.
	nsMu    sync.Mutex
	nsStore cache.Store
}

// RESTMapper is the slice of controller-runtime's mapper interface we need.
// A binding names a plural resource, so the dispatcher only has to confirm
// that the resource exists and is served — the GroupVersionResource it needs
// for the dynamic client is already spelled out in the binding.
type RESTMapper interface {
	KindFor(gvr schema.GroupVersionResource) (schema.GroupVersionKind, error)
}

// New creates an empty Dispatcher. Pass the cluster's dynamic client, a
// RESTMapper (controller-runtime's mgr.GetRESTMapper() can be wrapped to fit),
// and the Runner that runs the hook's prepared script. Every call runs on a
// fresh instance restored from that script (single shot); the runner is
// required so the worker can invoke it.
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
// instance via the Runner on every dispatch. bindings drive which informers
// start; they list and watch through as, the client of the hook's
// ServiceAccount (nil: the dispatcher's own client). emit (optional)
// publishes lifecycle events; pass nil in tests.
//
// If a subscription already exists for key, it is torn down first.
//
// d.mu only guards the subs map. The slow parts (stopping the old worker,
// waiting for informer sync) run under the per-key lock, so one hook that
// cannot sync never blocks Subscribe or Drop of another hook. Drop of the
// same key cancels a Subscribe that is still waiting; that Subscribe then
// returns an error.
// jshook.R17
// kube-access.R12
func (d *Dispatcher) Subscribe(parent context.Context, key types.NamespacedName, bindings []corev1alpha1.HookBinding, as dynamic.Interface, emit EventEmitter) error {
	if d.reg == nil {
		return fmt.Errorf("dispatcher: Runner is nil — Subscribe needs the runner to resolve live instances")
	}

	// Resolve everything that can fail before touching the old subscription:
	// a hook with a bad binding keeps the watches it already has.
	// jshook.R20
	watches, err := d.planWatches(bindings)
	if err != nil {
		return err
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

	if as == nil {
		as = d.dyn
	}
	for _, w := range watches {
		inNamespace, err := d.namespaceMatcher(ctx, w.binding.NamespaceSelector)
		if err != nil {
			return fail(fmt.Errorf("binding %q: namespaceSelector: %w", w.binding.Name, err))
		}
		if err := sub.startWatcher(ctx, as, w, inNamespace); err != nil {
			return fail(fmt.Errorf("binding %q: start watcher on %s: %w", w.binding.Name, w.gvr.Resource, err))
		}
	}
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("subscribe canceled: %w", err))
	}

	sub.wg.Add(1)
	go sub.runWorker(ctx)
	return nil
}

// watch is one informer to start: a binding narrowed to a single resource.
type watch struct {
	binding   corev1alpha1.HookBinding
	gvr       schema.GroupVersionResource
	objectSel labels.Selector
}

// planWatches expands every binding into one watch per resource and resolves
// everything that can fail: a binding is the cross product of apiGroups x
// apiVersions x resources, every combination must name a resource the cluster
// serves, and "*" is not a thing an informer can watch.
// jshook.R21
func (d *Dispatcher) planWatches(bindings []corev1alpha1.HookBinding) ([]watch, error) {
	var out []watch
	seen := map[string]string{}
	for _, b := range bindings {
		sel, err := metav1.LabelSelectorAsSelector(b.ObjectSelector)
		if err != nil {
			return nil, fmt.Errorf("binding %q: objectSelector: %w", b.Name, err)
		}
		for _, group := range b.APIGroups {
			for _, version := range b.APIVersions {
				for _, resource := range b.Resources {
					if group == "*" || version == "*" || resource == "*" {
						return nil, fmt.Errorf("binding %q: %q is not allowed in a hook binding — a watch needs a concrete apiGroup, apiVersion and resource", b.Name, "*")
					}
					gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
					if _, err := d.mapper.KindFor(gvr); err != nil {
						return nil, fmt.Errorf("binding %q: no resource %q in %q: %w", b.Name, resource, gvr.GroupVersion().String(), err)
					}
					if other, dup := seen[gvr.String()]; dup {
						return nil, fmt.Errorf("binding %q watches %s, which binding %q already watches — one event would call handle() twice", b.Name, gvr.Resource, other)
					}
					seen[gvr.String()] = b.Name
					out = append(out, watch{binding: b, gvr: gvr, objectSel: sel})
				}
			}
		}
	}
	return out, nil
}

// namespaceMatcher answers whether an object's namespace matches the
// binding's namespaceSelector. A nil selector matches everything, including
// cluster-scoped objects; any other selector needs namespace labels, so it
// never matches an object that has no namespace.
//
// The namespace informer is shared by every hook and started once, because
// namespace labels change rarely and a per-event GET would cost an API call
// per event.
// jshook.R22
func (d *Dispatcher) namespaceMatcher(ctx context.Context, sel *metav1.LabelSelector) (func(namespace string) bool, error) {
	if sel == nil {
		return func(string) bool { return true }, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return nil, err
	}
	store, err := d.namespaces(ctx)
	if err != nil {
		return nil, err
	}
	return func(namespace string) bool {
		if namespace == "" {
			return false
		}
		obj, ok, err := store.GetByKey(namespace)
		if err != nil || !ok {
			// The namespace is not in the cache: it was deleted, or the
			// cache has not caught up. Dropping is the safe answer — a hook
			// must not see objects of a namespace it did not select.
			return false
		}
		raw := toRaw(obj)
		if raw == nil {
			return false
		}
		md, _ := raw["metadata"].(map[string]any)
		set := labels.Set{}
		if l, ok := md["labels"].(map[string]any); ok {
			for k, v := range l {
				if vs, ok := v.(string); ok {
					set[k] = vs
				}
			}
		}
		return selector.Matches(set)
	}, nil
}

// namespaces returns the shared namespace cache, starting it on first use.
func (d *Dispatcher) namespaces(ctx context.Context) (cache.Store, error) {
	d.nsMu.Lock()
	defer d.nsMu.Unlock()
	if d.nsStore != nil {
		return d.nsStore, nil
	}
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(d.dyn, 0, metav1.NamespaceAll, nil)
	informer := factory.ForResource(gvr).Informer()
	// Detached from the subscribing hook's context: the cache is shared, so
	// dropping the hook that happened to start it must not stop it.
	factory.Start(context.WithoutCancel(ctx).Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return nil, fmt.Errorf("namespace cache sync canceled")
	}
	d.nsStore = informer.GetStore()
	return d.nsStore, nil
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
func (s *subscription) startWatcher(ctx context.Context, dyn dynamic.Interface, w watch, inNamespace func(string) bool) error {
	b := w.binding
	bindingName := b.Name

	// The object selector runs in the apiserver; the namespace selector
	// cannot (it matches labels of a different object) and is applied here.
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, metav1.NamespaceAll, func(o *metav1.ListOptions) {
		o.LabelSelector = w.objectSel.String()
	})
	informer := factory.ForResource(w.gvr).Informer()

	enqueueEvent := func(eventName string, raw map[string]any) {
		if !b.WantsEvent(corev1alpha1.HookEvent(eventName)) {
			return
		}
		md, _ := raw["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		if !inNamespace(ns) {
			return
		}
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

	syncEnabled := b.WantsSynchronization()

	gateMu.Lock()
	objs := informer.GetStore().List()
	syncObjects := make([]jshook.SyncObject, 0, len(objs))
	initialUIDs := make(map[string]bool, len(objs))
	for _, o := range objs {
		raw := toRaw(o)
		if raw == nil {
			continue
		}
		md, _ := raw["metadata"].(map[string]any)
		if ns, _ := md["namespace"].(string); !inNamespace(ns) {
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

// handleEvent runs one BindingContext through a fresh script instance and
// dispatches on the outcome. The locking + recover + OOM/cancel classification
// lives in jsrun.Runner.Invoke; the dispatcher only owns the policy table:
//
//   - panic (or wasm trap) → log + requeue; the next call still gets a fresh
//     instance, nothing to recover
//   - OOM / cancelled (timeout) → Warning + requeue
//   - other error → log + emit Warning + requeue
//   - ok → forget
//
// Cancellation: the runner bounds the call by spec.limits.timeoutSeconds and
// it surfaces as OutcomeCancelled. The engine stops the script through the
// QuickJS interrupt handler; the instance is thrown away either way, and the
// retried event runs on a fresh one from the same prepared script.
// jshook.R10
// jshook.R11
// jshook.R12
func (s *subscription) handleEvent(parent context.Context, logger logr.Logger, qkey eventKey, bc jshook.BindingContext) {
	console := &jslog.Collector{}
	out, res, err := jshook.Handle(jslog.WithSink(parent, console), s.reg, jsrun.HookKey(s.key), []jshook.BindingContext{bc})
	s.routeConsole(logger, console, bc)
	if err != nil {
		// ErrVMUnavailable: no script now (prepared again meanwhile), retry.
		// ErrUnknownKey: the hook was dropped (race with reconciler delete).
		if errors.Is(err, jsrun.ErrVMUnavailable) {
			s.noVM(logger, qkey, bc)
			return
		}
		logger.Info("hook not in runner — dropping event", "binding", bc.Binding, "event", bc.WatchEvent)
		s.queue.Forget(qkey)
		return
	}

	switch res.Outcome {
	case jsrun.OutcomePanic:
		logger.Error(fmt.Errorf("panic in handle(): %v", res.Panic),
			"handle() panicked",
			"binding", bc.Binding, "event", bc.WatchEvent, "stack", string(debug.Stack()))
		s.requeue(qkey, bc)

	case jsrun.OutcomeMemoryLimit:
		logger.Error(res.Err, "handle() hit memory limit",
			"binding", bc.Binding, "event", bc.WatchEvent)
		s.publishWarning(conditions.EventHandleFailed,
			"handle() exceeded its memory limit")
		s.requeue(qkey, bc)

	case jsrun.OutcomeCancelled:
		// The call's context ended. Only the per-call deadline is a timeout
		// of the hook and earns a Warning; a cancelled parent means
		// Subscribe or Drop is stopping this worker.
		if parent.Err() == nil {
			s.publishWarning(conditions.EventHandleTimeout,
				"handle() exceeded its timeout")
		}
		logger.Info("handle() cancelled",
			"binding", bc.Binding, "event", bc.WatchEvent, "elapsed", res.Duration)
		s.requeue(qkey, bc)

	case jsrun.OutcomeError:
		logger.Error(res.Err, "handle() failed", "binding", bc.Binding, "event", bc.WatchEvent)
		// Static message — the JS error string would explode dedup
		// cardinality at admission/event-loop volume. The verbose error
		// reaches the operator log only; the hook's own CR shows nothing
		// (STAT-4, EXEC-10).
		s.publishWarning(conditions.EventHandleFailed,
			"handle() returned an error")
		s.requeue(qkey, bc)

	default: // OutcomeOK
		if out != "" {
			logger.V(1).Info("handle() returned", "value", out)
		}
		s.queue.Forget(qkey)
	}
}

// routeConsole gives the script's own words a destination. Everything goes
// to the operator log; console.warn and console.error also become an Event on
// the hook, because that is the only surface the person who wrote the hook
// can read (EXEC-10).
//
// The Event message is author-controlled and therefore the one exception to
// the low-cardinality rule for Event messages. jslog caps the text and
// Collector.Route caps how many lines of one call become Events.
// js-execution.R17
func (s *subscription) routeConsole(logger logr.Logger, c *jslog.Collector, bc jshook.BindingContext) {
	c.Route(
		func(l jslog.Line) {
			logger.V(1).Info("console."+string(l.Level),
				"binding", bc.Binding, "event", bc.WatchEvent, "message", l.Text)
		},
		func(l jslog.Line) {
			s.publishWarning(conditions.EventScriptMessage, l.Text)
		},
	)
}

// noVM handles an event for a hook that has no VM now. A hook that is
// Preparing or Failed keeps its events: they are requeued with the rate limiter
// and meet the new VM. A hook the runner no longer knows loses them (Invoke
// reports ErrUnknownKey, handleEvent forgets the event).
// jshook.R19
func (s *subscription) noVM(logger logr.Logger, qkey eventKey, bc jshook.BindingContext) {
	logger.V(1).Info("hook has no VM yet — requeueing event",
		"binding", bc.Binding, "event", bc.WatchEvent)
	s.requeue(qkey, bc)
}

// publishWarning forwards to the EventEmitter the reconciler installed at
// Subscribe time. Everything the dispatcher has to say about a call is a
// Warning; the quiet path is the log. Nil-safe.
func (s *subscription) publishWarning(reason, message string) {
	if s.emit != nil {
		s.emit(corev1.EventTypeWarning, reason, message)
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
