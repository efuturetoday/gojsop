package jsregistry

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Backoff is the retry delay of a Broken key: Base doubles with every failed
// build in a row, is capped at Max and then spread by jitter. Zero fields use
// DefaultBackoffBase and DefaultBackoffMax.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// Defaults of Backoff.
const (
	DefaultBackoffBase = time.Second
	DefaultBackoffMax  = 5 * time.Minute
)

// Delay returns the wait before the retry that follows the attempts-th failed
// build: Base * 2^(attempts-1), capped at Max, then reduced by up to half
// (jitter), so the result lies in [d/2, d].
func (b Backoff) Delay(attempts int) time.Duration {
	if b.Base <= 0 {
		b.Base = DefaultBackoffBase
	}
	if b.Max <= 0 {
		b.Max = DefaultBackoffMax
	}
	if b.Max < b.Base {
		b.Max = b.Base
	}
	d := b.Base
	for i := 1; i < attempts && d < b.Max; i++ {
		d *= 2
	}
	if d > b.Max {
		d = b.Max
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// notifier collects the keys of one kind whose build ended. A key that is
// notified twice before it is read is delivered once; none is lost.
type notifier struct {
	mu      sync.Mutex
	pending map[Key]struct{}
	sig     chan struct{}
}

func newNotifier() *notifier {
	return &notifier{pending: make(map[Key]struct{}), sig: make(chan struct{}, 1)}
}

func (n *notifier) add(k Key) {
	n.mu.Lock()
	n.pending[k] = struct{}{}
	n.mu.Unlock()
	select {
	case n.sig <- struct{}{}:
	default:
	}
}

func (n *notifier) drain() []Key {
	n.mu.Lock()
	defer n.mu.Unlock()
	keys := make([]Key, 0, len(n.pending))
	for k := range n.pending {
		keys = append(keys, k)
		delete(n.pending, k)
	}
	return keys
}

func (r *Registry) notifierLocked(kind Kind) *notifier {
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
func (r *Registry) Watch(ctx context.Context, kind Kind) <-chan Key {
	r.mu.Lock()
	n := r.notifierLocked(kind)
	r.mu.Unlock()

	out := make(chan Key)
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
