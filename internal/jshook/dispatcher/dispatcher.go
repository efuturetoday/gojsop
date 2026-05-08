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
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jshook"
	"github.com/o-haase/gojsop/internal/jsregistry"
)

// timeoutStreakThreshold is how many consecutive Handle() calls may exceed
// Limits.TimeoutSeconds before the instance is rescue-restarted.
const timeoutStreakThreshold = 3

// eventKey identifies a queued BindingContext. All fields are strings, so the
// struct is hashable and can be used directly as a workqueue key — the queue
// dedupes by struct equality, collapsing bursts on the same object into one
// entry while still distinguishing Added/Modified/Deleted and old/new UIDs.
//
// For Synchronization-type contexts only `binding` and `event` are populated.
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
	reg    *jsregistry.Registry

	mu   sync.Mutex
	subs map[types.NamespacedName]*subscription
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
// and the Registry that owns per-hook persistent JS instances. The registry
// is required so that worker rescue paths (memory/panic/timeout) can rebuild
// the instance and the next call sees the new one transparently.
func New(dyn dynamic.Interface, mapper RESTMapper, reg *jsregistry.Registry) *Dispatcher {
	return &Dispatcher{
		dyn:    dyn,
		mapper: mapper,
		reg:    reg,
		subs:   make(map[types.NamespacedName]*subscription),
	}
}

// Subscribe (re)wires informers for hook `key`. The worker resolves the live
// instance via Registry on every dispatch. cfg drives which informers start.
//
// If a subscription already exists for key, it is torn down first.
func (d *Dispatcher) Subscribe(parent context.Context, key types.NamespacedName, cfg *jshook.Config) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.reg == nil {
		return fmt.Errorf("dispatcher: Registry is nil — Subscribe needs the registry to resolve live instances")
	}

	if old, ok := d.subs[key]; ok {
		old.stop()
		delete(d.subs, key)
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
	}

	for _, b := range cfg.Kubernetes {
		gv, err := schema.ParseGroupVersion(b.APIVersion)
		if err != nil {
			sub.stop()
			return fmt.Errorf("binding %q: parse apiVersion %q: %w", b.Name, b.APIVersion, err)
		}
		mapping, err := d.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: b.Kind}, gv.Version)
		if err != nil {
			sub.stop()
			return fmt.Errorf("binding %q: REST mapping for %s/%s: %w", b.Name, b.APIVersion, b.Kind, err)
		}
		if err := sub.startWatcher(ctx, d.dyn, mapping.Resource, b); err != nil {
			sub.stop()
			return fmt.Errorf("binding %q: start watcher: %w", b.Name, err)
		}
	}

	sub.wg.Add(1)
	go sub.runWorker(ctx)
	d.subs[key] = sub
	return nil
}

// Drop tears down all subscriptions for the given key.
func (d *Dispatcher) Drop(key types.NamespacedName) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if sub, ok := d.subs[key]; ok {
		sub.stop()
		delete(d.subs, key)
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
	reg    *jsregistry.Registry
	queue  workqueue.TypedRateLimitingInterface[eventKey]
	cancel context.CancelFunc
	wg     sync.WaitGroup

	pendMu  sync.Mutex
	pending map[eventKey]jshook.BindingContext

	// timeoutStreak counts consecutive Handle() calls that exceeded
	// Limits.TimeoutSeconds. Reset on a call that finishes inside the
	// budget. Reaching timeoutStreakThreshold triggers a rescue restart.
	// Owned by runWorker, which is single-goroutine per subscription.
	timeoutStreak int
}

// stop signals the worker and watchers to wind down, then blocks until the
// worker goroutine has actually exited. Subscribe() relies on this so that
// re-subscribing the same hook can never overlap an in-flight handle() call
// from the previous subscription.
func (s *subscription) stop() {
	s.queue.ShutDown()
	s.cancel()
	s.wg.Wait()
}

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

		out, err, elapsed, panicked, stack := s.invokeHandle(bc)

		switch {
		case panicked:
			// Panic in qjs glue or a host function escaped recover() inside
			// JS — instance state is suspect, rebuild it. The stack trace is
			// passed as a structured field rather than baked into err so
			// downstream log sinks can route or drop it independently.
			logger.Error(err, "handle() panicked — restarting instance",
				"binding", bc.Binding, "event", bc.WatchEvent, "stack", stack)
			s.rescue(logger, jsregistry.ReasonPanic)
			s.requeue(qkey, bc)
		case err != nil && jsengine.IsOOMError(err):
			// qjs MemoryLimit reached. The instance heap is corrupt from JS's
			// perspective; only a fresh runtime gets us back to a known good state.
			logger.Error(err, "handle() hit memory limit — restarting instance", "binding", bc.Binding, "event", bc.WatchEvent)
			s.rescue(logger, jsregistry.ReasonMemoryLimit)
			s.requeue(qkey, bc)
		case err != nil:
			logger.Error(err, "handle() failed", "binding", bc.Binding, "event", bc.WatchEvent)
			s.requeue(qkey, bc)
		default:
			if out != "" {
				logger.V(1).Info("handle() returned", "value", out)
			}
			s.queue.Forget(qkey)
		}

		// Timeout-streak tracking runs regardless of err — a hook that always
		// finishes overdue still deserves a kick eventually. We only consult
		// timeoutStreak after a successful or non-fatal failure path; rescue
		// paths above already rebuilt the instance and reset streak below.
		if mi, ok := s.reg.Get(s.key); ok {
			budget := time.Duration(mi.VM.Limits().TimeoutSeconds) * time.Second
			if budget > 0 && elapsed > budget {
				s.timeoutStreak++
				if s.timeoutStreak >= timeoutStreakThreshold {
					logger.Info("handle() exceeded timeout for streak threshold — restarting instance",
						"streak", s.timeoutStreak, "budget", budget, "elapsed", elapsed)
					s.rescue(logger, jsregistry.ReasonTimeoutStreak)
				}
			} else {
				s.timeoutStreak = 0
			}
		}

		s.queue.Done(qkey)
	}
}

// invokeHandle calls the live instance's Handle() with panic recovery and
// timing. When recover() catches something, panicked is true, err carries
// the panic value, and stack carries the runtime stack trace as a separate
// string so log sinks can route it independently of the error message.
func (s *subscription) invokeHandle(bc jshook.BindingContext) (out string, err error, elapsed time.Duration, panicked bool, stack string) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in handle(): %v", r)
			stack = string(debug.Stack())
			panicked = true
		}
	}()
	mi, ok := s.reg.Get(s.key)
	if !ok {
		return "", fmt.Errorf("hook %s not in registry", s.key), 0, false, ""
	}
	mi.CallMu.Lock()
	defer mi.CallMu.Unlock()
	start := time.Now()
	out, err = jshook.Handle(mi.VM, []jshook.BindingContext{bc})
	elapsed = time.Since(start)
	return out, err, elapsed, false, ""
}

// rescue rebuilds the instance via the registry and resets the timeout streak.
// On rebuild failure it logs and leaves the dead instance in place — the next
// reconcile will retry; the queue keeps eating events meanwhile.
func (s *subscription) rescue(logger logr.Logger, reason string) {
	if _, err := s.reg.RestartByKey(s.key, reason); err != nil {
		logger.Error(err, "rescue restart failed", "reason", reason)
		return
	}
	s.timeoutStreak = 0
}

// requeue stashes bc back under qkey (unless a fresher event arrived) and
// re-enqueues with rate-limited backoff.
func (s *subscription) requeue(qkey eventKey, bc jshook.BindingContext) {
	s.pendMu.Lock()
	if _, fresher := s.pending[qkey]; !fresher {
		s.pending[qkey] = bc
	}
	s.pendMu.Unlock()
	s.queue.AddRateLimited(qkey)
}
