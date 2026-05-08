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
		key:     key,
		reg:     d.reg,
		queue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), key.String()),
		cancel:  cancel,
		pending: make(map[string]jshook.BindingContext),
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
// The workqueue holds opaque string keys ("binding|event|ns/name|uid"); the
// associated BindingContext payload lives in `pending`. Two reasons:
//
//   - workqueue dedupes by hashing the queued item, but BindingContext contains
//     maps/slices and is therefore unhashable.
//   - keying on the object identity collapses bursts of Modified events for the
//     same resource into a single queue entry that always carries the freshest
//     snapshot — which is what hook authors expect.
type subscription struct {
	key    types.NamespacedName
	reg    *jsregistry.Registry
	queue  workqueue.RateLimitingInterface
	cancel context.CancelFunc

	pendMu  sync.Mutex
	pending map[string]jshook.BindingContext

	// timeoutStreak counts consecutive Handle() calls that exceeded
	// Limits.TimeoutSeconds. Reset on a call that finishes inside the
	// budget. Reaching timeoutStreakThreshold triggers a rescue restart.
	// Owned by runWorker, which is single-goroutine per subscription.
	timeoutStreak int
}

func (s *subscription) stop() {
	s.cancel()
	s.queue.ShutDown()
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
		qkey := bindingName + "|" + eventName + "|" + ns + "/" + name + "|" + uid
		bc := jshook.BindingContext{
			Binding:    bindingName,
			Type:       "Event",
			WatchEvent: eventName,
			Object:     raw,
		}
		s.pendMu.Lock()
		s.pending[qkey] = bc
		s.pendMu.Unlock()
		s.queue.Add(qkey)
	}

	// gate suppresses every handler call until Synchronization is published.
	// initialUIDs holds objects that were already in the snapshot — the next
	// AddFunc that fires for any of them is a redelivery from client-go's
	// sharedProcessor notification buffer (the queue can have pending Adds
	// at the moment WaitForCacheSync returns) and should be dropped to avoid
	// shipping the same object as Synchronization + Added.
	var (
		gateMu      sync.Mutex
		gateOpen    bool
		initialUIDs = map[string]bool{}
	)
	gated := func(eventName string, obj any) {
		raw := toRaw(obj)
		if raw == nil {
			return
		}
		uid := uidOf(raw)

		gateMu.Lock()
		if !gateOpen {
			gateMu.Unlock()
			return
		}
		if eventName == "Added" && uid != "" && initialUIDs[uid] {
			delete(initialUIDs, uid)
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
		// initialUIDs still gets cleared as those replays land, so a real
		// post-sync Add for the same object isn't dropped.
		for _, so := range syncObjects {
			delete(initialUIDs, uidOf(so.Object))
			enqueueEvent("Added", so.Object)
		}
	}
	gateOpen = true
	gateMu.Unlock()
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
	qkey := bindingName + "|Synchronization"
	bc := jshook.BindingContext{
		Binding: bindingName,
		Type:    "Synchronization",
		Objects: objs,
	}
	s.pendMu.Lock()
	s.pending[qkey] = bc
	s.pendMu.Unlock()
	s.queue.Add(qkey)
}

func (s *subscription) runWorker(ctx context.Context) {
	logger := log.FromContext(ctx).WithValues("hook", s.key.String())
	for {
		item, shutdown := s.queue.Get()
		if shutdown {
			return
		}
		qkey, ok := item.(string)
		if !ok {
			s.queue.Done(item)
			continue
		}
		s.pendMu.Lock()
		bc, present := s.pending[qkey]
		delete(s.pending, qkey)
		s.pendMu.Unlock()
		if !present {
			s.queue.Done(item)
			continue
		}

		out, err, elapsed, panicked := s.invokeHandle(bc)

		switch {
		case panicked:
			// Panic in qjs glue or a host function escaped recover() inside
			// JS — instance state is suspect, rebuild it.
			logger.Error(err, "handle() panicked — restarting instance", "binding", bc.Binding, "event", bc.WatchEvent)
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
// timing. The returned panicked flag is true when recover() caught something;
// in that case err carries the panic value formatted as an error.
func (s *subscription) invokeHandle(bc jshook.BindingContext) (out string, err error, elapsed time.Duration, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in handle(): %v\n%s", r, debug.Stack())
			panicked = true
		}
	}()
	mi, ok := s.reg.Get(s.key)
	if !ok {
		return "", fmt.Errorf("hook %s not in registry", s.key), 0, false
	}
	mi.CallMu.Lock()
	defer mi.CallMu.Unlock()
	start := time.Now()
	out, err = jshook.Handle(mi.VM, []jshook.BindingContext{bc})
	elapsed = time.Since(start)
	return out, err, elapsed, false
}

// rescue rebuilds the instance via the registry and resets the timeout streak.
// On rebuild failure it logs and leaves the dead instance in place — the next
// reconcile will retry; the queue keeps eating events meanwhile.
func (s *subscription) rescue(logger logr.Logger, reason string) {
	if _, err := s.reg.RestartByKey(s.key, reason, nil); err != nil {
		logger.Error(err, "rescue restart failed", "reason", reason)
		return
	}
	s.timeoutStreak = 0
}

// requeue stashes bc back under qkey (unless a fresher event arrived) and
// re-enqueues with rate-limited backoff.
func (s *subscription) requeue(qkey string, bc jshook.BindingContext) {
	s.pendMu.Lock()
	if _, fresher := s.pending[qkey]; !fresher {
		s.pending[qkey] = bc
	}
	s.pendMu.Unlock()
	s.queue.AddRateLimited(qkey)
}
