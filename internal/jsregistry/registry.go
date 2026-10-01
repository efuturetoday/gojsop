// Package jsregistry holds the prepared script of every JSHook/JSAdmission the
// controller knows about: one snapshot per jsrun.Key (kind and name), owned by
// the controller's lifetime. Every call runs in a fresh VM restored from that
// snapshot (single shot) and is thrown away afterwards. It is the first
// adapter of the script execution port jsrun.Runner; callers use the port,
// never this package (js-execution.R10).
package jsregistry

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// historyCap bounds the recovery log so a flapping key can't drive registry
// memory unboundedly. 20 lines up roughly with what an operator can read in
// `kubectl describe` and matches RecentRestarts on the CRD status.
const historyCap = 20

// DefaultMaxConcurrentCalls is the process-wide number of calls that may run
// at once when Options.MaxConcurrentCalls is zero.
const DefaultMaxConcurrentCalls = 16

// Prepared is the registry's view of a prepared script: the snapshot every
// call starts from, and what callers see of it through jsrun.State. Opts is
// cached so the registry can compare a later spec against it.
//
// js-registry.R9
type Prepared struct {
	PreparedAt time.Time
	// Limits are the effective limits (defaults applied).
	Limits jsrun.Limits
	// Meta is the value the PostBuildHook returned.
	Meta any
	// Recoveries is the recovery log as of the install of this script.
	Recoveries jsrun.Recoveries

	Snapshot *jsengine.Snapshot
	Opts     jsrun.Spec
}

// Options configure a Registry.
type Options struct {
	// MaxConcurrentCalls bounds the calls that run at once over all keys.
	// Every call holds a VM of its own (a few MB, up to its memory limit), so
	// this bounds the memory of a burst. Zero means DefaultMaxConcurrentCalls.
	MaxConcurrentCalls int
}

// Registry holds the prepared script of every JSHook/JSAdmission the
// controller knows about. One entry per Key (kind and name), owned by the
// controller's lifetime.
//
// Every key is in exactly one state (see State): Ready with a snapshot,
// Building, or Broken with the last build error. Builds run asynchronously,
// one goroutine per key (Ensure); nothing waits for them, a controller is told
// through Watch when one finishes.
//
// Concurrency model: r.mu protects the map and the per-key state only; it is
// never held while user JS executes (build or call). Per-key build
// serialization is done via slot.buildMu. Calls share nothing: each restores
// its own VM from the immutable snapshot, so calls of one key run in parallel,
// bounded only by the process-wide call semaphore.
type Registry struct {
	mu        sync.Mutex
	slots     map[jsrun.Key]*slot
	notifiers map[jsrun.Kind]*notifier

	// calls is the process-wide call semaphore.
	//
	// js-registry.R20
	calls chan struct{}
}

// slot is the state of one key. A key is Building while building is set,
// else Ready while prep is set, else Broken.
type slot struct {
	// buildMu serializes builds of this key.
	//
	// js-registry.R7
	buildMu sync.Mutex

	prep *Prepared // installed script; nil while Broken and before the first build

	// opts are the options of the running build, or of the last attempt.
	opts     jsrun.Spec
	building bool
	gen      uint64             // bumped for every started or cancelled build
	cancel   context.CancelFunc // cancels the running build

	// Broken state.
	err      error
	attempts int
	nextTry  time.Time

	// recovery log, carried over every installed script.
	restarts map[jsrun.RecoveryReason]int32
	history  []jsrun.Recovery
}

// ConfigureEngine sets up the process-wide JS engine before the first VM, so the
// compilation of engine.wasm happens at start-up. cacheDir keeps the compiled
// machine code across restarts (empty: in memory only, see
// jsengine.Options.CacheDir). Without a call the engine starts on first use.
func ConfigureEngine(cacheDir string) error {
	return jsengine.Configure(jsengine.Options{CacheDir: cacheDir})
}

// NewRegistry returns a registry with the default options.
func NewRegistry() *Registry { return New(Options{}) }

// New returns a registry with o.
func New(o Options) *Registry {
	n := o.MaxConcurrentCalls
	if n <= 0 {
		n = DefaultMaxConcurrentCalls
	}
	return &Registry{
		slots:     make(map[jsrun.Key]*slot),
		notifiers: make(map[jsrun.Kind]*notifier),
		calls:     make(chan struct{}, n),
	}
}

func (s *slot) state() jsrun.State {
	switch {
	case s.building:
		return jsrun.State{Phase: jsrun.PhasePreparing}
	case s.prep != nil:
		return jsrun.State{Phase: jsrun.PhaseReady, PreparedAt: s.prep.PreparedAt, Meta: s.prep.Meta, Recoveries: s.prep.Recoveries}
	default:
		return jsrun.State{Phase: jsrun.PhaseFailed, Err: s.err, Attempts: s.attempts, NextAttempt: s.nextTry}
	}
}

// abortBuild cancels the running build, if any. Its result is discarded.
func (s *slot) abortBuild() {
	if s.building {
		s.cancel()
		s.gen++
		s.building = false
	}
}

// build prepares key: a fresh VM loads the source, the snapshot is taken right
// after the top-level eval, then PostBuild runs on the same VM (its effects,
// config() included, do not reach the snapshot), and the VM is closed. The
// caller must hold the per-key build mutex; r.mu MUST NOT be held because
// LoadModule and PostBuild execute user JS that may block or take a long time.
//
// js-registry.R6
// js-execution.R16
func (r *Registry) build(ctx context.Context, key jsrun.Key, opts jsrun.Spec) (*Prepared, error) {
	vm, err := jsengine.New(opts.Limits)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", jsrun.ErrNewVM, err)
	}
	defer vm.Close()
	if opts.Host != nil {
		binder, ok := opts.Host.(jsengine.HostBinder)
		if !ok {
			return nil, fmt.Errorf("%w: host %T is not a jsengine.HostBinder", jsrun.ErrBindHost, opts.Host)
		}
		if err := vm.BindHost(binder); err != nil {
			return nil, fmt.Errorf("%w: %v", jsrun.ErrBindHost, err)
		}
	}
	if err := vm.LoadModule(ctx, key.Name.Name+".js", string(opts.Source)); err != nil {
		return nil, fmt.Errorf("%w: %v", jsrun.ErrLoadModule, err)
	}
	snap, err := vm.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: snapshot: %v", jsrun.ErrLoadModule, err)
	}
	var extra any
	if opts.PostBuild != nil {
		extra, err = opts.PostBuild(ctx, vm)
		if err != nil {
			// Pass MissingExportError through unchanged so errors.As works at
			// the reconciler. Other PostBuild errors (config() threw, bad
			// shape, ...) get tagged with ErrPostBuild for classification.
			var miss *jsrun.MissingExportError
			if errors.As(err, &miss) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %v", jsrun.ErrPostBuild, err)
		}
	}
	return &Prepared{
		PreparedAt: time.Now(),
		Limits:     opts.Limits.WithDefaults(),
		Meta:       extra,
		Snapshot:   snap,
		Opts:       opts,
	}, nil
}

// optsChanged reports whether opts needs a different script than cur: another
// source hash, other effective limits (zero fields count as the defaults) or
// another reset token. Host, PostBuild and Backoff cannot be compared or do not
// shape the script; they follow the source.
func optsChanged(cur, opts jsrun.Spec) bool {
	return cur.SourceHash != opts.SourceHash ||
		cur.Limits.WithDefaults() != opts.Limits.WithDefaults() ||
		cur.ResetToken != opts.ResetToken
}

// Ensure reports the state of key for opts and never waits for a build:
//
//   - Ready: a script matching opts (source hash and effective limits) is prepared.
//   - Building: a build for opts runs (started by this call if none did).
//   - Broken: the last build for opts failed. The next build starts once the
//     backoff of opts.Backoff has passed; opts that differ from the failed
//     ones start a build at once.
//
// At most one build runs per key. If opts change while a build runs, the
// running build is cancelled through its context and a new one starts. The
// build is bounded by the timeout limit. When it ends, the key is notified on
// the channel of Watch.
//
// js-registry.R3
// js-registry.R5
// js-registry.R15
// js-registry.R16
// js-registry.R17
func (r *Registry) Ensure(key jsrun.Key, opts jsrun.Spec) jsrun.State {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := r.slots[key]
	if s == nil {
		s = &slot{}
		r.slots[key] = s
	}

	switch {
	case s.prep != nil && !optsChanged(s.prep.Opts, opts):
		// The installed script is what opts ask for (also when a build for
		// other options runs: the change was taken back).
		s.abortBuild()
		return s.state()
	case s.building && !optsChanged(s.opts, opts):
		return s.state()
	case !s.building && s.prep == nil && s.err != nil && !optsChanged(s.opts, opts):
		if time.Now().Before(s.nextTry) {
			return s.state()
		}
		// retry: attempts keep counting
	default:
		s.attempts = 0
		s.err = nil
	}

	var reason jsrun.RecoveryReason
	if s.prep != nil {
		switch {
		case s.prep.Opts.SourceHash != opts.SourceHash:
			reason = jsrun.ReasonSourceChanged
		case s.prep.Opts.Limits.WithDefaults() != opts.Limits.WithDefaults():
			reason = jsrun.ReasonLimitsChanged
		default:
			reason = jsrun.ReasonManual // only the reset token differs
		}
	}
	s.abortBuild()
	s.opts = opts
	r.startBuildLocked(key, s, reason)
	return s.state()
}

// startBuildLocked starts the build goroutine for s.opts. r.mu must be held.
func (r *Registry) startBuildLocked(key jsrun.Key, s *slot, reason jsrun.RecoveryReason) {
	timeout := time.Duration(s.opts.Limits.WithDefaults().TimeoutSeconds) * time.Second
	// The build outlives the reconcile that asked for it, so it derives from
	// a background context, bounded by the timeout limit.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	s.building = true
	s.gen++
	s.cancel = cancel
	go r.runBuild(ctx, cancel, key, s, s.gen, s.opts, reason)
}

// runBuild prepares opts under the per-key build lock and installs the
// result, or marks the key Broken. A build that was cancelled or whose key was
// dropped in the meantime is discarded.
//
// js-registry.R6
// js-registry.R7
// js-registry.R18
func (r *Registry) runBuild(ctx context.Context, cancel context.CancelFunc, key jsrun.Key, s *slot, gen uint64, opts jsrun.Spec, reason jsrun.RecoveryReason) {
	defer cancel()

	s.buildMu.Lock()
	defer s.buildMu.Unlock()

	var p *Prepared
	err := ctx.Err()
	if err == nil {
		p, err = r.build(ctx, key, opts)
	}

	r.mu.Lock()
	if r.slots[key] != s || s.gen != gen {
		r.mu.Unlock()
		return
	}
	s.building = false
	if err != nil {
		s.prep = nil
		s.err = err
		s.attempts++
		s.nextTry = time.Now().Add(opts.Backoff.Delay(s.attempts))
	} else {
		r.installLocked(s, p, reason)
	}
	n := r.notifierLocked(key.Kind)
	r.mu.Unlock()

	n.add(key)
}

// installLocked makes p the script of s, carries the recovery log over and
// appends an entry if reason is set (the very first build of a key has no
// transition to record). r.mu must be held.
//
// js-registry.R11
// status-conditions.R4
func (r *Registry) installLocked(s *slot, p *Prepared, reason jsrun.RecoveryReason) {
	if reason != "" {
		next := make(map[jsrun.RecoveryReason]int32, len(s.restarts)+1)
		maps.Copy(next, s.restarts)
		next[reason]++
		s.restarts = next

		hist := s.history
		if len(hist) >= historyCap {
			hist = hist[len(hist)-historyCap+1:]
		}
		s.history = append(append([]jsrun.Recovery(nil), hist...), jsrun.Recovery{Time: time.Now(), Reason: reason})
	}
	p.Recoveries = jsrun.Recoveries{ByReason: s.restarts, Recent: s.history}

	s.prep = p
	s.err = nil
	s.attempts = 0
}

// lookup returns the prepared script of key (nil if none) and whether the key
// is known at all.
func (r *Registry) lookup(key jsrun.Key) (p *Prepared, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.slots[key]
	if s == nil {
		return nil, false
	}
	return s.prep, true
}

// Get returns the prepared script of key without modifying anything.
func (r *Registry) Get(key jsrun.Key) (*Prepared, bool) {
	p, _ := r.lookup(key)
	return p, p != nil
}

// Drop forgets key and cancels a running build. Idempotent; called from the
// reconciler when the resource is deleted. Calls that already run finish on
// their own VMs.
//
// js-registry.R8
func (r *Registry) Drop(key jsrun.Key) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.slots[key]; s != nil {
		s.abortBuild()
		delete(r.slots, key)
	}
}

// acquire takes a slot of the call semaphore, or fails when ctx ends first.
//
// js-registry.R20
func (r *Registry) acquire(ctx context.Context) error {
	select {
	case r.calls <- struct{}{}:
		return nil
	default:
	}
	select {
	case r.calls <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: waiting for a free call slot: %w", jsengine.ErrCancelled, ctx.Err())
	}
}

func (r *Registry) release() { <-r.calls }

// Call runs fn on a fresh VM restored from the snapshot of key, with a
// panic-safe wrapper, and closes the VM afterwards. The result classifies the
// outcome (panic / OOM / cancelled / other error / ok) so callers don't
// reimplement the same recover + errors.Is dance. A wasm trap
// (jsengine.TrapError) counts as a panic. Nothing has to be recovered after
// any outcome: the VM is thrown away. Call never waits for a build: it
// returns ErrVMUnavailable at once while the key holds no script (Building or
// Broken), and ErrUnknownKey if the key is not registered at all.
//
// The call first waits for a slot of the process-wide call semaphore; a ctx
// that ends while it waits ends the call with OutcomeCancelled.
// jsrun.Result.Duration is wall-clock from the start of the wait to fn
// return, so a saturated semaphore shows up as long durations.
//
// js-execution.R2
// js-registry.R1
// js-registry.R2
// js-registry.R20
func (r *Registry) Call(ctx context.Context, key jsrun.Key, fn func(ctx context.Context, vm *jsengine.VM) error) (res jsrun.Result, err error) {
	p, known := r.lookup(key)
	if p == nil {
		if known {
			return jsrun.Result{}, fmt.Errorf("%w: %s", jsrun.ErrVMUnavailable, key)
		}
		return jsrun.Result{}, fmt.Errorf("%w: %s", jsrun.ErrUnknownKey, key)
	}

	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()
	if err := r.acquire(ctx); err != nil {
		return jsrun.Result{Outcome: jsrun.OutcomeCancelled, Err: err}, nil
	}
	defer r.release()

	vm, err := p.Snapshot.NewVM(ctx)
	if err != nil {
		return jsrun.Result{}, fmt.Errorf("%w: %s: restore: %v", jsrun.ErrVMUnavailable, key, err)
	}
	defer vm.Close()

	defer func() {
		if p := recover(); p != nil {
			res.Outcome = jsrun.OutcomePanic
			res.Panic = p
			res.Err = nil
		}
	}()
	err = fn(ctx, vm)
	var trap *jsengine.TrapError
	switch {
	case err == nil:
		res.Outcome = jsrun.OutcomeOK
	case errors.As(err, &trap):
		res.Outcome = jsrun.OutcomePanic
		res.Panic = err
	case errors.Is(err, jsengine.ErrCancelled), ctx.Err() != nil:
		res.Outcome = jsrun.OutcomeCancelled
		res.Err = err
	case errors.Is(err, jsengine.ErrOOM):
		res.Outcome = jsrun.OutcomeMemoryLimit
		res.Err = err
	default:
		res.Outcome = jsrun.OutcomeError
		res.Err = err
	}
	return res, nil
}

// Invoke implements jsrun.Runner: it calls export on a fresh VM of key through
// Call, so semaphore, panic recovery and outcome classification are Call's.
// The call is bounded by the timeout limit of the script; the wait for the
// semaphore counts against it.
//
// js-execution.R2
// js-execution.R3
// js-registry.R1
// js-registry.R2
func (r *Registry) Invoke(ctx context.Context, key jsrun.Key, export string, in, out any) (jsrun.Result, error) {
	if p, _ := r.lookup(key); p != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(p.Limits.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	return r.Call(ctx, key, func(ctx context.Context, vm *jsengine.VM) error {
		return vm.Invoke(ctx, export, in, out)
	})
}

// State returns the state of key without starting anything, and false if the
// key is unknown. It is not part of the port (no caller needs it): tests read
// the state of a key with it.
func (r *Registry) State(key jsrun.Key) (jsrun.State, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.slots[key]
	if s == nil {
		return jsrun.State{}, false
	}
	return s.state(), true
}

// Registry is the first adapter of the script execution port: it is both the
// data path (Runner) and the lifecycle path (Scripts).
var (
	_ jsrun.Runner  = (*Registry)(nil)
	_ jsrun.Scripts = (*Registry)(nil)
)

// Len reports how many prepared scripts the registry is holding (test/metrics).
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.slots {
		if s.prep != nil {
			n++
		}
	}
	return n
}
