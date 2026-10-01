package jsregistry

import (
	"context"
	"sync"

	"github.com/o-haase/gojsop/internal/jsrun"
)

// notifier collects the keys of one kind whose build ended. A key that is
// notified twice before it is read is delivered once; none is lost.
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

func (r *Registry) notifierLocked(kind jsrun.Kind) *notifier {
	n := r.notifiers[kind]
	if n == nil {
		n = newNotifier()
		r.notifiers[kind] = n
	}
	return n
}

// Watch returns the channel on which the registry reports keys of kind whose
// build ended, whether it installed a VM or failed. A controller of that kind
// reads it and reconciles the key. Notifications that arrived before Watch
// are kept. Use one Watch per kind; the channel closes when ctx ends.
//
// js-registry.R18
func (r *Registry) Watch(ctx context.Context, kind jsrun.Kind) <-chan jsrun.Key {
	r.mu.Lock()
	n := r.notifierLocked(kind)
	r.mu.Unlock()

	out := make(chan jsrun.Key)
	go func() {
		defer close(out)
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
