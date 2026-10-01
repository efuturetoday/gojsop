package jsrun

import (
	"math/rand/v2"
	"time"
)

// Backoff is the retry delay of a Failed key: Base doubles with every failed
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
