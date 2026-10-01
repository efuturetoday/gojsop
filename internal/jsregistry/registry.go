// Package jsregistry holds the live JS VM for every JSHook/JSAdmission the
// controller knows about. One VM per jsrun.Key (kind and name), owned by the
// controller's lifetime. It is the first adapter of the script execution port
// jsrun.Runner; callers use the port, never this package (js-execution.R10).
package jsregistry

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// historyCap bounds ManagedVM.History so a flapping VM can't drive registry
// memory unboundedly. 20 lines up roughly with what an operator can read in
// `kubectl describe` and matches RecentRestarts on the CRD status.
const historyCap = 20

// ManagedVM is the registry's view of a per-resource persistent VM. Opts is
// cached so the registry can rebuild the VM after a rescue restart without
// bouncing through the controller. The embedded Instance is the part callers
// see through jsrun.Runner.
//
// js-registry.R9
type ManagedVM struct {
	jsrun.Instance
	VM   *jsengine.VM
	Opts jsrun.Options

	// CallMu serializes calls into the qjs runtime. qjs is not goroutine-safe,
	// and a JSHook + JSAdmission could converge on the same VM in a future
	// phase. Today the dispatcher and the admission server both go through
	// Registry.Invoke, which acquires this mutex.
	CallMu sync.Mutex
	// closed is set under CallMu when the VM is closed; a call that took the
	// lock afterwards reports ErrVMUnavailable instead of running on it.
	closed bool
}

// Registry holds the live JS VM for every JSHook/JSAdmission the controller
// knows about. One entry per Key (kind and name), owned by the controller's
// lifetime.
//
// Every key is in exactly one state (see State): Ready with a VM, Building, or
// Broken with the last build error. Builds run asynchronously, one goroutine
// per key (Ensure); nothing waits for them, a controller is told through
// Watch when one finishes.
//
// Concurrency model: r.mu protects the map and the per-key state only; it is
// never held while user JS executes (LoadModule, postBuild, Call). Per-key
// build serialization is done via slot.buildMu: a goroutine holds it while
// running the expensive VM construction, so builds of one key never overlap
// and builds of different keys never block each other. Per-VM call
// serialization is done via ManagedVM.CallMu, which Registry.Call takes
// around the user fn.
type Registry struct {
	mu        sync.Mutex
	slots     map[jsrun.Key]*slot
	notifiers map[jsrun.Kind]*notifier
}

// slot is the state of one key. A key is Building while building is set,
// else Ready while vm is set, else Broken.
type slot struct {
	// buildMu serializes builds and restarts of this key.
	//
	// js-registry.R7
	buildMu sync.Mutex

	vm *ManagedVM // installed VM; nil while Broken and before the first build

	// opts are the options of the running build, or of the last attempt.
	opts     jsrun.Options
	building bool
	gen      uint64             // bumped for every started or cancelled build
	cancel   context.CancelFunc // cancels the running build

	// Broken state.
	err      error
	attempts int
	nextTry  time.Time

	// restart log, carried over every installed VM.
	restarts map[jsrun.RestartReason]int32
	history  []jsrun.RestartEvent
}

func NewRegistry() *Registry {
	return &Registry{
		slots:     make(map[jsrun.Key]*slot),
		notifiers: make(map[jsrun.Kind]*notifier),
	}
}

func (s *slot) state() jsrun.State {
	switch {
	case s.building:
		return jsrun.State{Kind: jsrun.StateBuilding}
	case s.vm != nil:
		return jsrun.State{Kind: jsrun.StateReady, Instance: &s.vm.Instance}
	default:
		return jsrun.State{Kind: jsrun.StateBroken, Err: s.err, Attempts: s.attempts, NextTry: s.nextTry}
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

// build constructs and initializes a fresh VM for key with the given options.
// On any failure the partially-built VM is closed. The caller must hold the
// per-key build mutex (see getBuildLock); r.mu MUST NOT be held because
// LoadModule and PostBuild execute user JS that may block or take a long time.
//
// ctx is the build / reconcile context, plumbed into wazero so a stuck
// LoadModule or PostBuild can be cancelled by a controller shutdown or a
// reconcile deadline.
//
// js-registry.R6
func (r *Registry) build(ctx context.Context, key jsrun.Key, opts jsrun.Options) (*ManagedVM, error) {
	vm, err := jsengine.New(opts.Limits)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", jsrun.ErrNewVM, err)
	}
	if opts.Host != nil {
		binder, ok := opts.Host.(jsengine.HostBinder)
		if !ok {
			vm.Close()
			return nil, fmt.Errorf("%w: host %T is not a jsengine.HostBinder", jsrun.ErrBindHost, opts.Host)
		}
		if err := vm.BindHost(binder); err != nil {
			vm.Close()
			return nil, fmt.Errorf("%w: %v", jsrun.ErrBindHost, err)
		}
	}
	if err := vm.LoadModule(ctx, key.Name.Name+".js", string(opts.Source)); err != nil {
		vm.Close()
		return nil, fmt.Errorf("%w: %v", jsrun.ErrLoadModule, err)
	}
	var extra any
	if opts.PostBuild != nil {
		extra, err = opts.PostBuild(ctx, vm)
		if err != nil {
			vm.Close()
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
	return &ManagedVM{
		Instance: jsrun.Instance{
			StartedAt: time.Now(),
			Limits:    opts.Limits.WithDefaults(),
			Extra:     extra,
		},
		VM:   vm,
		Opts: opts,
	}, nil
}

// optsChanged reports whether opts needs a different VM than cur: another
// source hash or other effective limits (zero fields count as the defaults).
// Host, PostBuild and Backoff cannot be compared or do not shape the VM;
// they follow the source.
func optsChanged(cur, opts jsrun.Options) bool {
	return cur.SourceHash != opts.SourceHash ||
		cur.Limits.WithDefaults() != opts.Limits.WithDefaults()
}

// Ensure reports the state of key for opts and never waits for a build:
//
//   - Ready: a VM matching opts (source hash and effective limits) is installed.
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
func (r *Registry) Ensure(key jsrun.Key, opts jsrun.Options) jsrun.State {
	r.mu.Lock()
	defer r.mu.Unlock()

	s := r.slots[key]
	if s == nil {
		s = &slot{}
		r.slots[key] = s
	}

	switch {
	case s.vm != nil && !optsChanged(s.vm.Opts, opts):
		// The installed VM is what opts ask for (also when a build for other
		// options runs: the change was taken back).
		s.abortBuild()
		return s.state()
	case s.building && !optsChanged(s.opts, opts):
		return s.state()
	case !s.building && s.vm == nil && s.err != nil && !optsChanged(s.opts, opts):
		if time.Now().Before(s.nextTry) {
			return s.state()
		}
		// retry: attempts keep counting
	default:
		s.attempts = 0
		s.err = nil
	}

	var reason jsrun.RestartReason
	if s.vm != nil {
		reason = jsrun.ReasonSourceChanged
		if s.vm.Opts.SourceHash == opts.SourceHash {
			reason = jsrun.ReasonLimitsChanged
		}
	}
	s.abortBuild()
	s.opts = opts
	r.startBuildLocked(key, s, reason)
	return s.state()
}

// startBuildLocked starts the build goroutine for s.opts. r.mu must be held.
func (r *Registry) startBuildLocked(key jsrun.Key, s *slot, reason jsrun.RestartReason) {
	timeout := time.Duration(s.opts.Limits.WithDefaults().TimeoutSeconds) * time.Second
	// The build outlives the reconcile that asked for it, so it derives from
	// a background context, bounded by the timeout limit.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	s.building = true
	s.gen++
	s.cancel = cancel
	go r.runBuild(ctx, cancel, key, s, s.gen, s.opts, reason)
}

// runBuild builds a VM for opts under the per-key build lock and installs it,
// or marks the key Broken. A build that was cancelled or whose key was dropped
// in the meantime is discarded.
//
// js-registry.R6
// js-registry.R7
// js-registry.R18
func (r *Registry) runBuild(ctx context.Context, cancel context.CancelFunc, key jsrun.Key, s *slot, gen uint64, opts jsrun.Options, reason jsrun.RestartReason) {
	defer cancel()

	s.buildMu.Lock()
	defer s.buildMu.Unlock()

	var mi *ManagedVM
	err := ctx.Err()
	if err == nil {
		mi, err = r.build(ctx, key, opts)
	}

	r.mu.Lock()
	if r.slots[key] != s || s.gen != gen {
		r.mu.Unlock()
		if mi != nil {
			mi.VM.Close() // never visible to a caller
		}
		return
	}
	s.building = false
	var old *ManagedVM
	if err != nil {
		old = s.vm
		s.vm = nil
		s.err = err
		s.attempts++
		s.nextTry = time.Now().Add(opts.Backoff.Delay(s.attempts))
	} else {
		old = r.installLocked(s, mi, reason)
	}
	n := r.notifierLocked(key.Kind)
	r.mu.Unlock()

	closeVM(old)
	n.add(key)
}

// closeVM closes a displaced VM once no call runs on it.
//
// js-registry.R8
func closeVM(mi *ManagedVM) {
	if mi != nil {
		mi.CallMu.Lock()
		mi.closed = true
		mi.VM.Close()
		mi.CallMu.Unlock()
	}
}

// installLocked makes mi the VM of s, carries the restart log over and
// appends a RestartEvent if reason is set (the very first build of a key has
// no transition to record). Returns the displaced VM (or nil) so the caller
// can drain CallMu before Close. r.mu must be held.
//
// js-registry.R11
// status-conditions.R4
func (r *Registry) installLocked(s *slot, mi *ManagedVM, reason jsrun.RestartReason) *ManagedVM {
	if reason != "" {
		next := make(map[jsrun.RestartReason]int32, len(s.restarts)+1)
		maps.Copy(next, s.restarts)
		next[reason]++
		s.restarts = next

		hist := s.history
		if len(hist) >= historyCap {
			hist = hist[len(hist)-historyCap+1:]
		}
		s.history = append(append([]jsrun.RestartEvent(nil), hist...), jsrun.RestartEvent{Time: time.Now(), Reason: reason})
	}
	mi.RestartsByReason = s.restarts
	mi.History = s.history

	old := s.vm
	s.vm = mi
	s.err = nil
	s.attempts = 0
	return old
}

// lookup returns the installed VM of key (nil if none) and whether the key is
// known at all.
func (r *Registry) lookup(key jsrun.Key) (mi *ManagedVM, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.slots[key]
	if s == nil {
		return nil, false
	}
	return s.vm, true
}

// Known reports whether the registry holds an entry for key, in any state.
// With Get it tells "no VM yet or any more" (retry later) from "never
// registered or dropped" (give up).
func (r *Registry) Known(key jsrun.Key) bool {
	_, known := r.lookup(key)
	return known
}

// Get returns the current ManagedVM for key without modifying anything. The
// dispatcher's worker uses this to look up the live VM per dispatch, so a
// rescue restart transparently switches the next call to the new VM.
func (r *Registry) Get(key jsrun.Key) (*ManagedVM, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.slots[key]
	if s == nil || s.vm == nil {
		return nil, false
	}
	return s.vm, true
}

// Drop closes and forgets the VM for key and cancels a running build.
// Idempotent; called from the reconciler when the resource is deleted. Drop
// drains any in-flight call (by acquiring CallMu) before vm.Close so we don't
// race active wasm.
//
// js-registry.R8
func (r *Registry) Drop(key jsrun.Key) {
	logger := log.Log.WithName("jsregistry").WithValues("key", key.String())
	r.mu.Lock()
	var mi *ManagedVM
	if s := r.slots[key]; s != nil {
		s.abortBuild()
		mi = s.vm
		delete(r.slots, key)
	}
	r.mu.Unlock()

	logger.Info("drop entered", "vmFound", mi != nil)
	if mi != nil {
		mi.CallMu.Lock()
		logger.Info("drop closing vm")
		start := time.Now()
		mi.closed = true
		mi.VM.Close()
		logger.Info("drop closed vm", "duration", time.Since(start))
		mi.CallMu.Unlock()
	}
}

// Restart is the rescue and manual restart path: it closes the VM of key
// and starts a rebuild from the cached jsrun.Options, and returns without
// waiting for it. Until the build ends the key is Building and Registry.Call
// reports ErrVMUnavailable; a failed build leaves it Broken, holding no VM.
// The key is notified now and again when the build ends, so its controller
// publishes both states. Returns ErrUnknownKey if the resource is unknown to
// the registry.
//
// The old VM's CallMu is acquired before vm.Close so we don't race in-flight
// wasm; callers run it after their own call returned. The build runs under
// the context and deadline of Ensure.
//
// js-registry.R4
// js-registry.R13
// js-registry.R19
func (r *Registry) Restart(key jsrun.Key, reason jsrun.RestartReason) error {
	r.mu.Lock()
	s := r.slots[key]
	if s == nil {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", jsrun.ErrUnknownKey, key)
	}
	old := s.vm
	if old != nil {
		s.opts = old.Opts
	} else if s.building {
		// A build for the same key already runs; it replaces the VM anyway.
		r.mu.Unlock()
		return nil
	}
	s.vm = nil
	s.abortBuild()
	s.attempts, s.err = 0, nil
	r.startBuildLocked(key, s, reason)
	n := r.notifierLocked(key.Kind)
	r.mu.Unlock()

	closeVM(old)
	n.add(key)
	return nil
}

// Call runs fn under mi.CallMu with a panic-safe wrapper. The result classifies
// the outcome (panic / OOM / cancelled / other error / ok) so callers don't
// reimplement the same recover + errors.Is dance. Call never waits for a build:
// it returns ErrVMUnavailable at once while the key holds no VM (Building or
// Broken), and ErrUnknownKey if the key is not registered at all.
//
// fn receives the same ctx Call was called with; pass a context with deadline
// to enforce a per-call timeout (wazero observes the runtime's embedded ctx
// because qjs.New was constructed with CloseOnContextDone).
//
// jsrun.Result.Duration is wall-clock from CallMu acquisition to fn return —
// the lock-wait time is included intentionally so a "stuck" VM shows up as
// long durations on whichever caller queues behind the offender.
//
// js-execution.R2
// js-registry.R1
// js-registry.R19
func (r *Registry) Call(ctx context.Context, key jsrun.Key, fn func(ctx context.Context, vm *jsengine.VM) error) (jsrun.Result, *ManagedVM, error) {
	mi, known := r.lookup(key)
	if mi == nil {
		if known {
			return jsrun.Result{}, nil, fmt.Errorf("%w: %s", jsrun.ErrVMUnavailable, key)
		}
		return jsrun.Result{}, nil, fmt.Errorf("%w: %s", jsrun.ErrUnknownKey, key)
	}

	mi.CallMu.Lock()
	defer mi.CallMu.Unlock()
	if mi.closed { // replaced or dropped while this call waited for the lock
		return jsrun.Result{}, nil, fmt.Errorf("%w: %s", jsrun.ErrVMUnavailable, key)
	}

	res := jsrun.Result{}
	start := time.Now()
	func() {
		defer func() {
			if p := recover(); p != nil {
				res.Outcome = jsrun.OutcomePanic
				res.Panic = p
			}
		}()
		err := fn(ctx, mi.VM)
		switch {
		case err == nil:
			res.Outcome = jsrun.OutcomeOK
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
	}()
	res.Duration = time.Since(start)
	return res, mi, nil
}

// Invoke implements jsrun.Runner: it calls export on the VM of key through
// Call, so lock, panic recovery and outcome classification are Call's.
//
// js-execution.R2
// js-registry.R1
func (r *Registry) Invoke(ctx context.Context, key jsrun.Key, export string, in, out any) (jsrun.Result, error) {
	res, _, err := r.Call(ctx, key, func(ctx context.Context, vm *jsengine.VM) error {
		return vm.Invoke(ctx, export, in, out)
	})
	return res, err
}

// Instance implements jsrun.Runner: the callers' view of the VM of key.
func (r *Registry) Instance(key jsrun.Key) (*jsrun.Instance, bool) {
	mi, ok := r.Get(key)
	if !ok {
		return nil, false
	}
	return &mi.Instance, true
}

// Registry is the first adapter of the script execution port.
var _ jsrun.Runner = (*Registry)(nil)

// Len reports how many live VMs the registry is holding (test/metrics).
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.slots {
		if s.vm != nil {
			n++
		}
	}
	return n
}
