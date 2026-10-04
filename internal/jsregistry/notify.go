package jsregistry

import (
	"context"
	"sync"

	"github.com/efuturetoday/gojsop/internal/jsrun"
)

// notifier collects the keys of one subscriber whose build ended. A key that
// is notified twice before it is read is delivered once; none is lost.
type notifier struct {
	mu      sync.Mutex
	pending map[jsrun.Key]struct{}
	sig     chan struct{}
}

func newNotifier() *notifier {
	return &notifier{pending: make(map[jsrun.Key]struct{}), sig: make(chan struct{}, 1)}
}

func (n *notifier) add(k jsrun.Key) {
	n.mu.Lock()
	n.pending[k] = struct{}{}
	n.mu.Unlock()
	select {
	case n.sig <- struct{}{}:
	default:
	}
}

func (n *notifier) drain() []jsrun.Key {
	n.mu.Lock()
	defer n.mu.Unlock()
	keys := make([]jsrun.Key, 0, len(n.pending))
	for k := range n.pending {
		keys = append(keys, k)
		delete(n.pending, k)
	}
	return keys
}

// fanout holds every subscriber of one kind plus the backlog that a
// subscriber which registers later inherits.
//
// Each subscriber has a notifier of its own, so a subscriber that reads a
// notification cannot take it away from another. That matters because the
// admission kind has two readers: the leader-only status controller and the
// server controller that runs on every replica.
type fanout struct {
	subs map[*notifier]struct{}
	// backlog is every key of this kind whose build has ended at least once,
	// minus the keys dropped since. A subscriber registering after a build
	// finished is seeded from it, so a notification is never lost just
	// because the reader was not up yet. It holds one entry per live key, so
	// it is bounded by the number of resources.
	backlog map[jsrun.Key]struct{}
}

func newFanout() *fanout {
	return &fanout{subs: make(map[*notifier]struct{}), backlog: make(map[jsrun.Key]struct{})}
}

// fanoutLocked returns the fanout of kind, creating it on first use.
// r.mu must be held.
func (r *Registry) fanoutLocked(kind jsrun.Kind) *fanout {
	f := r.notifiers[kind]
	if f == nil {
		f = newFanout()
		r.notifiers[kind] = f
	}
	return f
}

// notifyLocked records key in the backlog of its kind and returns every
// subscriber that has to be told. The caller calls add outside r.mu, because
// a notifier takes a lock of its own.
// r.mu must be held.
func (r *Registry) notifyLocked(key jsrun.Key) []*notifier {
	f := r.fanoutLocked(key.Kind)
	f.backlog[key] = struct{}{}
	subs := make([]*notifier, 0, len(f.subs))
	for n := range f.subs {
		subs = append(subs, n)
	}
	return subs
}

// forgetLocked drops key from the backlog of its kind, so a subscriber that
// registers after the key was dropped is not seeded with it.
// r.mu must be held.
func (r *Registry) forgetLocked(key jsrun.Key) {
	if f := r.notifiers[key.Kind]; f != nil {
		delete(f.backlog, key)
	}
}

// subscribe registers a notifier for kind, seeded with the backlog.
func (r *Registry) subscribe(kind jsrun.Kind) *notifier {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.fanoutLocked(kind)
	n := newNotifier()
	for k := range f.backlog {
		n.add(k)
	}
	f.subs[n] = struct{}{}
	return n
}

// unsubscribe removes n from kind. Idempotent.
func (r *Registry) unsubscribe(kind jsrun.Kind, n *notifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := r.notifiers[kind]; f != nil {
		delete(f.subs, n)
	}
}

// Watch returns a channel on which the registry reports keys of kind whose
// build ended, whether it installed a VM or failed. Every call returns a
// channel of its own and every channel sees every build, so a kind may have
// more than one reader. Notifications that arrived before Watch are kept. The
// channel closes when ctx ends, and the subscription goes with it.
//
// js-registry.R18
// js-registry.R21
func (r *Registry) Watch(ctx context.Context, kind jsrun.Kind) <-chan jsrun.Key {
	n := r.subscribe(kind)

	out := make(chan jsrun.Key)
	go func() {
		defer close(out)
		defer r.unsubscribe(kind, n)
		for {
			select {
			case <-ctx.Done():
				return
			case <-n.sig:
			}
			for _, k := range n.drain() {
				select {
				case out <- k:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
