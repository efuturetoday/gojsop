// Package jsregistry holds the live JS VM for every JSHook/JSAdmission the
// controller knows about. One VM per NamespacedName, owned by the controller's
// lifetime.
package jsregistry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/o-haase/gojsop/internal/jsengine"
)

// RestartReason is the documented enum of triggers that cause a managed VM to
// be rebuilt. Recorded into JSHook.status.instance.lastRestartReason.
type RestartReason string

// Restart reasons. Every value is wired to a real trigger:
//   - ReasonSourceChanged: spec.source's hash differs on Reconcile (Registry.GetOrLoad)
//   - ReasonMemoryLimit:   handle() returned an "out of memory" error (dispatcher worker / admission)
//   - ReasonPanic:         handle() panicked (Registry.Call recover)
//   - ReasonTimeout:       handle() exceeded the per-call deadline (admission, dispatcher)
//   - ReasonTimeoutStreak: dispatcher saw N consecutive timeouts (legacy; still used by dispatcher)
//   - ReasonManual:        gojsop.io/restart annotation changed on the JSHook (reconciler)
const (
	ReasonSourceChanged RestartReason = "source-changed"
	ReasonMemoryLimit   RestartReason = "memory-limit"
	ReasonPanic         RestartReason = "panic"
	ReasonTimeout       RestartReason = "timeout"
	ReasonTimeoutStreak RestartReason = "timeout-streak"
	ReasonManual        RestartReason = "manual"
)

// PostBuildHook is invoked once per fresh VM after the source has loaded. It
// returns the value to stash on ManagedVM.Extra. JSHook uses it to call
// jshook.ReadConfig and cache the parsed Config. Returning a non-nil error
// aborts the load and closes the VM.
//
// ctx is the build / reconcile context — config() runs once per build, so
// the hook's reconcile deadline applies.
type PostBuildHook func(ctx context.Context, vm *jsengine.VM) (extra any, err error)

// BuildOptions bundles every input the registry needs to build (or rebuild)
// a VM. Caching the whole struct on ManagedVM lets RestartByKey rebuild
// without callers having to re-supply source, limits or the host binder —
// the dispatcher's rescue path doesn't have those in hand.
type BuildOptions struct {
	Source     []byte
	SourceHash string
	Limits     jsengine.Limits
	Binder     jsengine.HostBinder
	PostBuild  PostBuildHook
}

// historyCap bounds ManagedVM.History so a flapping VM can't drive registry
// memory unboundedly. 20 lines up roughly with what an operator can read in
// `kubectl describe` and matches RecentRestarts on the CRD status.
const historyCap = 20

// RestartEvent records one transition from old VM to new for a key. The slice
// of these on ManagedVM is the authoritative restart log; per-reason counters
// are an aggregate cache.
type RestartEvent struct {
	Time   time.Time
	Reason RestartReason
	// Err carries the diagnostic for rescue-driven restarts (panic / OOM /
	// timeout / timeout-streak). Empty for source-changed and manual.
	Err string
}

// ManagedVM is the registry's view of a per-resource persistent VM. Opts is
// cached so the registry can rebuild the VM after a rescue restart without
// bouncing through the controller.
type ManagedVM struct {
	VM        *jsengine.VM
	Opts      BuildOptions
	StartedAt time.Time
	// RestartsByReason aggregates History so callers don't recompute it; the
	// two are kept in sync by installNew.
	RestartsByReason map[RestartReason]int32
	// History is the restart log (oldest first), capped at historyCap. Empty
	// on the very first build of a key.
	History []RestartEvent
	// Extra is an opaque feature-specific payload the loader can stash on
	// the ManagedVM at build time (e.g. JSHook caches its parsed Config
	// here so reconciles don't re-enter the JS runtime concurrently with
	// dispatcher handle() calls). The registry never touches it.
	Extra any

	// CallMu serializes calls into the qjs runtime. qjs is not goroutine-safe,
	// and a JSHook + JSAdmission could converge on the same VM in a future
	// phase. Today the dispatcher and the admission server both go through
	// Registry.Call, which acquires this mutex.
	CallMu sync.Mutex
}

// LastRestart returns the most recent RestartEvent, or the zero value if the
// VM has never been restarted (first build).
func (m *ManagedVM) LastRestart() RestartEvent {
	if len(m.History) == 0 {
		return RestartEvent{}
	}
	return m.History[len(m.History)-1]
}

// CallOutcome classifies how a Registry.Call ran. The classification owns the
// decision tree previously open-coded in the dispatcher and admission server.
type CallOutcome int

const (
	OutcomeOK          CallOutcome = iota // fn returned nil
	OutcomePanic                          // fn panicked (recovered)
	OutcomeMemoryLimit                    // fn returned a wrapped jsengine.ErrOOM
	OutcomeCancelled                      // ctx.Err() != nil — wazero deadline / cancellation tripped
	OutcomeError                          // any other non-nil error from fn
)

// CallResult is what Registry.Call hands back. Outcome drives the rescue /
// HTTP-status decision; Duration is for metrics + the dispatcher's streak
// counter; Panic / Err carry the diagnostic detail.
type CallResult struct {
	Outcome  CallOutcome
	Duration time.Duration
	Panic    any   // populated when Outcome == OutcomePanic
	Err      error // populated when Outcome != OK / Panic
}

// Registry holds the live JS VM for every JSHook/JSAdmission the controller
// knows about. One VM per NamespacedName, owned by the controller's lifetime.
//
// Concurrency model: r.mu protects map structural integrity only — it is
// never held while user JS executes (LoadModule, postBuild, Call). Per-key
// build serialization is done via buildLocks: a goroutine holds the per-key
// lock while running the expensive VM construction, so two reconciles for
// the same key can't race, but reconciles for different keys never block
// each other. Per-VM call serialization is done via ManagedVM.CallMu, which
// Registry.Call takes around the user fn.
type Registry struct {
	mu         sync.Mutex
	vms        map[types.NamespacedName]*ManagedVM
	buildLocks map[types.NamespacedName]*sync.Mutex
}

func NewRegistry() *Registry {
	return &Registry{
		vms:        make(map[types.NamespacedName]*ManagedVM),
		buildLocks: make(map[types.NamespacedName]*sync.Mutex),
	}
}

// getBuildLock returns the per-key build mutex, creating it on first use.
// The map of build locks is itself protected by r.mu, but the returned
// mutex is acquired by the caller without holding r.mu — that is the whole
// point of the split.
func (r *Registry) getBuildLock(key types.NamespacedName) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	bMu, ok := r.buildLocks[key]
	if !ok {
		bMu = &sync.Mutex{}
		r.buildLocks[key] = bMu
	}
	return bMu
}

// build constructs and initializes a fresh VM for key with the given options.
// On any failure the partially-built VM is closed. The caller must hold the
// per-key build mutex (see getBuildLock); r.mu MUST NOT be held because
// LoadModule and PostBuild execute user JS that may block or take a long time.
//
// ctx is the build / reconcile context, plumbed into wazero so a stuck
// LoadModule or PostBuild can be cancelled by a controller shutdown or a
// reconcile deadline.
func (r *Registry) build(ctx context.Context, key types.NamespacedName, opts BuildOptions) (*ManagedVM, error) {
	vm, err := jsengine.New(opts.Limits)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNewVM, err)
	}
	if opts.Binder != nil {
		if err := vm.BindHost(opts.Binder); err != nil {
			vm.Close()
			return nil, fmt.Errorf("%w: %v", ErrBindHost, err)
		}
	}
	if err := vm.LoadModule(ctx, key.Name+".js", string(opts.Source)); err != nil {
		vm.Close()
		return nil, fmt.Errorf("%w: %v", ErrLoadModule, err)
	}
	var extra any
	if opts.PostBuild != nil {
		extra, err = opts.PostBuild(ctx, vm)
		if err != nil {
			vm.Close()
			// Pass MissingExportError through unchanged so errors.As works at
			// the reconciler. Other PostBuild errors (config() threw, bad
			// shape, ...) get tagged with ErrPostBuild for classification.
			var miss *MissingExportError
			if errors.As(err, &miss) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %v", ErrPostBuild, err)
		}
	}
	return &ManagedVM{
		VM:        vm,
		Opts:      opts,
		StartedAt: time.Now(),
		Extra:     extra,
	}, nil
}

// GetOrLoad returns the live ManagedVM for key. If nothing exists yet, or if
// opts.SourceHash differs from the last load, a fresh QuickJS runtime is
// started, the source is evaluated, and the previous VM (if any) is closed.
// The boolean reports whether a (re)start happened on this call.
//
// Implements double-checked locking around a per-key build mutex:
//  1. fast path under r.mu — return existing if hash matches
//  2. acquire per-key build mutex (no r.mu held); concurrent reconciles for
//     other keys race straight through
//  3. re-check under r.mu in case a peer just finished building
//  4. run build() with no global lock held — user JS executes here
//  5. install the new VM under r.mu, after taking the old VM's CallMu so
//     no in-flight call races vm.Close()
func (r *Registry) GetOrLoad(ctx context.Context, key types.NamespacedName, opts BuildOptions) (*ManagedVM, bool, error) {
	// 1. fast path
	r.mu.Lock()
	if existing, ok := r.vms[key]; ok && existing.Opts.SourceHash == opts.SourceHash {
		r.mu.Unlock()
		return existing, false, nil
	}
	r.mu.Unlock()

	// 2. serialize builds for this key only
	bMu := r.getBuildLock(key)
	bMu.Lock()
	defer bMu.Unlock()

	// 3. double-check after acquiring the build lock
	r.mu.Lock()
	if existing, ok := r.vms[key]; ok && existing.Opts.SourceHash == opts.SourceHash {
		r.mu.Unlock()
		return existing, false, nil
	}
	r.mu.Unlock()

	// 4. heavy lifting — runs user JS, no global lock held
	mi, err := r.build(ctx, key, opts)
	if err != nil {
		return nil, false, err
	}

	// 5. install. Swap under r.mu, then drain the old VM's CallMu before
	//    Close so we don't race active wasm.
	old := r.installNew(key, mi, ReasonSourceChanged, nil)
	if old != nil {
		old.CallMu.Lock()
		old.VM.Close()
		old.CallMu.Unlock()
	}
	return mi, true, nil
}

// installNew swaps mi in for key, propagates the previous restart log, and
// appends a new RestartEvent if old != nil (the very first build of a key
// has no event — there's no transition to record). Returns the displaced
// ManagedVM (or nil) so the caller can drain CallMu before Close.
//
// reason / err are the trigger for *this* transition; they are appended to
// History and bump RestartsByReason[reason] by one.
func (r *Registry) installNew(key types.NamespacedName, mi *ManagedVM, reason RestartReason, err error) *ManagedVM {
	r.mu.Lock()
	defer r.mu.Unlock()

	old := r.vms[key]
	if old != nil {
		mi.RestartsByReason = make(map[RestartReason]int32, len(old.RestartsByReason)+1)
		for k, v := range old.RestartsByReason {
			mi.RestartsByReason[k] = v
		}
		mi.RestartsByReason[reason]++

		ev := RestartEvent{Time: time.Now(), Reason: reason}
		if err != nil {
			ev.Err = err.Error()
		}
		hist := old.History
		if len(hist) >= historyCap {
			hist = hist[len(hist)-historyCap+1:]
		}
		mi.History = append(append([]RestartEvent(nil), hist...), ev)
	}
	r.vms[key] = mi
	return old
}

// Get returns the current ManagedVM for key without modifying anything. The
// dispatcher's worker uses this to look up the live VM per dispatch, so a
// rescue restart transparently switches the next call to the new VM.
func (r *Registry) Get(key types.NamespacedName) (*ManagedVM, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mi, ok := r.vms[key]
	return mi, ok
}

// Drop closes and forgets the VM for key. Idempotent; called from the
// reconciler when the resource is deleted. Drop drains any in-flight call
// (by acquiring CallMu) before vm.Close so we don't race active wasm.
//
// The build lock is dropped too; in the unlikely case a concurrent
// GetOrLoad is mid-build for the same key it will finish and install a
// zombie VM, which the next reconcile (NotFound → Drop) cleans up.
func (r *Registry) Drop(key types.NamespacedName) {
	logger := log.Log.WithName("jsregistry").WithValues("key", key.String())
	r.mu.Lock()
	mi := r.vms[key]
	delete(r.vms, key)
	delete(r.buildLocks, key)
	r.mu.Unlock()

	logger.Info("drop entered", "vmFound", mi != nil)
	if mi != nil {
		mi.CallMu.Lock()
		logger.Info("drop closing vm")
		start := time.Now()
		mi.VM.Close()
		logger.Info("drop closed vm", "duration", time.Since(start))
		mi.CallMu.Unlock()
	}
}

// RestartByKey force-replaces the VM for key with a fresh runtime, reusing the
// cached BuildOptions from the last successful load. Used by rescue paths
// (memory/panic/timeout/manual) that don't have the build inputs in hand.
// Returns ErrUnknownKey if the resource is unknown to the registry.
//
// Holds the per-key build mutex across the rebuild so a concurrent
// GetOrLoad for the same key serializes behind it; r.mu is only held for
// the brief read of the existing entry and the install at the end. The
// old VM's CallMu is acquired before vm.Close so we don't race in-flight
// wasm.
//
// The build context comes from a background context: rescue is driven by
// the dispatcher / admission server, not by a per-reconcile deadline, and
// blocking the rescue on a request context that's about to be cancelled
// would mean every timeout-rescue starts from a half-built VM.
func (r *Registry) RestartByKey(key types.NamespacedName, reason RestartReason) (*ManagedVM, error) {
	bMu := r.getBuildLock(key)
	bMu.Lock()
	defer bMu.Unlock()

	r.mu.Lock()
	existing, ok := r.vms[key]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}

	mi, err := r.build(context.Background(), key, existing.Opts)
	if err != nil {
		return nil, err
	}

	old := r.installNew(key, mi, reason, nil)
	if old != nil {
		old.CallMu.Lock()
		old.VM.Close()
		old.CallMu.Unlock()
	}
	return mi, nil
}

// Call runs fn under mi.CallMu with a panic-safe wrapper. The result classifies
// the outcome (panic / OOM / cancelled / other error / ok) so callers don't
// reimplement the same recover + errors.Is dance. Returns ErrUnknownKey if
// no VM is registered for key.
//
// fn receives the same ctx Call was called with; pass a context with deadline
// to enforce a per-call timeout (wazero observes the runtime's embedded ctx
// because qjs.New was constructed with CloseOnContextDone).
//
// CallResult.Duration is wall-clock from CallMu acquisition to fn return —
// the lock-wait time is included intentionally so a "stuck" VM shows up as
// long durations on whichever caller queues behind the offender.
//
// Block: js-execution R2
func (r *Registry) Call(ctx context.Context, key types.NamespacedName, fn func(ctx context.Context, vm *jsengine.VM) error) (CallResult, *ManagedVM, error) {
	mi, ok := r.Get(key)
	if !ok {
		return CallResult{}, nil, fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}

	mi.CallMu.Lock()
	defer mi.CallMu.Unlock()

	res := CallResult{}
	start := time.Now()
	func() {
		defer func() {
			if p := recover(); p != nil {
				res.Outcome = OutcomePanic
				res.Panic = p
			}
		}()
		err := fn(ctx, mi.VM)
		switch {
		case err == nil:
			res.Outcome = OutcomeOK
		case errors.Is(err, jsengine.ErrCancelled), ctx.Err() != nil:
			res.Outcome = OutcomeCancelled
			res.Err = err
		case errors.Is(err, jsengine.ErrOOM):
			res.Outcome = OutcomeMemoryLimit
			res.Err = err
		default:
			res.Outcome = OutcomeError
			res.Err = err
		}
	}()
	res.Duration = time.Since(start)
	return res, mi, nil
}

// Len reports how many live VMs the registry is holding (test/metrics).
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.vms)
}
