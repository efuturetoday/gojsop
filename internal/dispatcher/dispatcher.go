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

	jsruntime "github.com/o-haase/gojsop/internal/runtime"
)

// timeoutStreakThreshold is how many consecutive Handle() calls may exceed
// Resources.TimeoutSeconds before the instance is rescue-restarted.
const timeoutStreakThreshold = 3

// Dispatcher routes Kubernetes events into per-hook JS handlers.
type Dispatcher struct {
	dyn    dynamic.Interface
	mapper RESTMapper
	reg    *jsruntime.Registry

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
func New(dyn dynamic.Interface, mapper RESTMapper, reg *jsruntime.Registry) *Dispatcher {
	return &Dispatcher{
		dyn:    dyn,
		mapper: mapper,
		reg:    reg,
		subs:   make(map[types.NamespacedName]*subscription),
	}
}

// Subscribe (re)wires informers for hook `key`. The worker resolves the live
// instance via Registry on every dispatch — passing `inst` here is no longer
// required for that lookup; it is accepted for symmetry with the old API and
// is unused internally. cfg drives which informers are started.
//
// If a subscription already exists for key, it is torn down first.
func (d *Dispatcher) Subscribe(parent context.Context, key types.NamespacedName, _ *jsruntime.Instance, cfg *jsruntime.Config) error {
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
		pending: make(map[string]jsruntime.BindingContext),
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
	reg    *jsruntime.Registry
	queue  workqueue.RateLimitingInterface
	cancel context.CancelFunc

	pendMu  sync.Mutex
	pending map[string]jsruntime.BindingContext

	// timeoutStreak counts consecutive Handle() calls that exceeded
	// Resources.TimeoutSeconds. Reset on a call that finishes inside the
	// budget. Reaching timeoutStreakThreshold triggers a rescue restart.
	// Owned by runWorker, which is single-goroutine per subscription.
	timeoutStreak int
}

func (s *subscription) stop() {
	s.cancel()
	s.queue.ShutDown()
}

func (s *subscription) startWatcher(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, b jsruntime.KubernetesBinding) error {
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

	enqueue := func(eventName string, obj any) {
		if !wantedEvents[eventName] {
			return
		}
		raw, ok := obj.(map[string]any)
		if !ok {
			// dynamic informer returns *unstructured.Unstructured — extract its map.
			if u, ok := obj.(interface{ UnstructuredContent() map[string]any }); ok {
				raw = u.UnstructuredContent()
			}
		}
		md, _ := raw["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		name, _ := md["name"].(string)
		uid, _ := md["uid"].(string)
		qkey := bindingName + "|" + eventName + "|" + ns + "/" + name + "|" + uid
		bc := jsruntime.BindingContext{
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

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { enqueue("Added", o) },
		UpdateFunc: func(_, n any) { enqueue("Modified", n) },
		DeleteFunc: func(o any) { enqueue("Deleted", o) },
	})
	if err != nil {
		return err
	}

	factory.Start(ctx.Done())
	// We don't block on cache sync here — events that arrive during sync are
	// queued anyway. shell-operator handles initial state via "Synchronization"
	// type contexts, which we can layer in once the dispatcher is stable.
	return nil
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
			s.rescue(logger, jsruntime.ReasonPanic)
			s.requeue(qkey, bc)
		case err != nil && jsruntime.IsOOMError(err):
			// qjs MemoryLimit reached. The instance heap is corrupt from JS's
			// perspective; only a fresh runtime gets us back to a known good state.
			logger.Error(err, "handle() hit memory limit — restarting instance", "binding", bc.Binding, "event", bc.WatchEvent)
			s.rescue(logger, jsruntime.ReasonMemoryLimit)
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
			budget := time.Duration(mi.Instance.Resources().TimeoutSeconds) * time.Second
			if budget > 0 && elapsed > budget {
				s.timeoutStreak++
				if s.timeoutStreak >= timeoutStreakThreshold {
					logger.Info("handle() exceeded timeout for streak threshold — restarting instance",
						"streak", s.timeoutStreak, "budget", budget, "elapsed", elapsed)
					s.rescue(logger, jsruntime.ReasonTimeoutStreak)
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
func (s *subscription) invokeHandle(bc jsruntime.BindingContext) (out string, err error, elapsed time.Duration, panicked bool) {
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
	out, err = mi.Instance.Handle([]jsruntime.BindingContext{bc})
	elapsed = time.Since(start)
	return out, err, elapsed, false
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
func (s *subscription) requeue(qkey string, bc jsruntime.BindingContext) {
	s.pendMu.Lock()
	if _, fresher := s.pending[qkey]; !fresher {
		s.pending[qkey] = bc
	}
	s.pendMu.Unlock()
	s.queue.AddRateLimited(qkey)
}
