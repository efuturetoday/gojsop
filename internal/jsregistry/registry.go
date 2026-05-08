// Package jsregistry holds the live JS VM for every JSHook/JSAdmission the
// controller knows about. One VM per NamespacedName, owned by the controller's
// lifetime.
package jsregistry

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/jsengine"
)

// Restart reasons recorded into JSHook.status.instance.lastRestartReason.
// This is the CRD's documented enum — every value is wired to a real trigger:
//   - ReasonSourceChanged: spec.source's hash differs on Reconcile (Registry.GetOrLoad)
//   - ReasonMemoryLimit:   handle() returned an "out of memory" error (dispatcher worker)
//   - ReasonPanic:         handle() panicked (dispatcher worker recover)
//   - ReasonTimeoutStreak: handle() exceeded TimeoutSeconds N times in a row (dispatcher worker)
//   - ReasonManual:        gojsop.io/restart annotation changed on the JSHook (reconciler)
const (
	ReasonSourceChanged = "source-changed"
	ReasonMemoryLimit   = "memory-limit"
	ReasonPanic         = "panic"
	ReasonTimeoutStreak = "timeout-streak"
	ReasonManual        = "manual"
)

// ManagedVM is the registry's view of a per-resource persistent VM. Source and
// limits are cached so the registry can rebuild the VM after a rescue restart
// without bouncing through the controller.
type ManagedVM struct {
	VM           *jsengine.VM
	SourceHash   string
	StartedAt    time.Time
	RestartCount int32
	LastReason   string
	// Extra is an opaque feature-specific payload the loader can stash on
	// the ManagedVM at build time (e.g. JSHook caches its parsed Config
	// here so reconciles don't re-enter the JS runtime concurrently with
	// dispatcher handle() calls). The registry never touches it.
	Extra any

	// source and limits are the inputs needed to rebuild this VM after a
	// rescue restart (memory/panic/timeout/manual). They are not exported
	// because callers must go through Registry.RestartByKey to mutate them.
	source []byte
	limits jsengine.Limits

	// CallMu serializes calls into the qjs runtime. qjs is not goroutine-safe,
	// and a JSHook + JSAdmission can converge on the same VM in phase 2.
	// Today the dispatcher takes it before Handle and the admission server
	// takes it before HandleAdmission; with one role per CR contention is
	// effectively zero.
	CallMu sync.Mutex
}

// PostBuildHook is invoked once per fresh VM after the source has loaded. It
// returns the value to stash on ManagedVM.Extra. JSHook uses it to call
// jshook.ReadConfig and cache the parsed Config. Returning a non-nil error
// aborts the load and closes the VM.
type PostBuildHook func(vm *jsengine.VM) (extra any, err error)

// Registry holds the live JS VM for every JSHook/JSAdmission the controller
// knows about. One VM per NamespacedName, owned by the controller's lifetime.
//
// All methods are safe for concurrent use. The expectation is that the
// reconciler holds the per-hook lock implicitly via controller-runtime's
// per-key serialization, so contention here is rare.
type Registry struct {
	// Binder, if non-nil, is invoked on every newly created VM before the
	// user's module is evaluated. Use this to register host functions
	// (e.g. globalThis.kube) onto the runtime.
	Binder jsengine.HostBinder

	mu  sync.Mutex
	vms map[types.NamespacedName]*ManagedVM
}

func NewRegistry() *Registry {
	return &Registry{vms: make(map[types.NamespacedName]*ManagedVM)}
}

// build constructs and initializes a fresh VM for key with the given source
// and limits. On any failure the partially-built VM is closed. Caller holds
// r.mu. The optional postBuild lets the feature attach its own typed payload
// (e.g. parsed config()) onto ManagedVM.Extra.
func (r *Registry) build(key types.NamespacedName, source []byte, sourceHash string, lim jsengine.Limits, postBuild PostBuildHook) (*ManagedVM, error) {
	vm, err := jsengine.New(lim)
	if err != nil {
		return nil, fmt.Errorf("registry: new VM: %w", err)
	}
	if err := vm.BindHost(r.Binder); err != nil {
		vm.Close()
		return nil, fmt.Errorf("registry: bind host: %w", err)
	}
	if err := vm.LoadModule(key.Name+".js", string(source)); err != nil {
		vm.Close()
		return nil, fmt.Errorf("registry: load module: %w", err)
	}
	var extra any
	if postBuild != nil {
		extra, err = postBuild(vm)
		if err != nil {
			vm.Close()
			return nil, fmt.Errorf("registry: post-build: %w", err)
		}
	}
	return &ManagedVM{
		VM:         vm,
		SourceHash: sourceHash,
		StartedAt:  time.Now(),
		Extra:      extra,
		source:     source,
		limits:     lim,
	}, nil
}

// GetOrLoad returns the live ManagedVM for key. If nothing exists yet, or if
// sourceHash differs from the last load, a fresh QuickJS runtime is started,
// the source is evaluated, and the previous VM (if any) is closed. The
// boolean reports whether a (re)start happened on this call.
func (r *Registry) GetOrLoad(key types.NamespacedName, source []byte, sourceHash string, lim jsengine.Limits, postBuild PostBuildHook) (*ManagedVM, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.vms[key]
	if ok && existing.SourceHash == sourceHash {
		return existing, false, nil
	}

	mi, err := r.build(key, source, sourceHash, lim, postBuild)
	if err != nil {
		return nil, false, err
	}
	if ok {
		existing.VM.Close()
		mi.RestartCount = existing.RestartCount + 1
		mi.LastReason = ReasonSourceChanged
	}
	r.vms[key] = mi
	return mi, true, nil
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
// reconciler when the resource is deleted.
func (r *Registry) Drop(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mi, ok := r.vms[key]; ok {
		mi.VM.Close()
		delete(r.vms, key)
	}
}

// RestartByKey force-replaces the VM for key with a fresh runtime, reusing the
// cached source/limits from the last successful load. Used by rescue paths
// (memory/panic/timeout/manual) that don't have the source bytes in hand.
// Returns an error if the resource is unknown to the registry.
func (r *Registry) RestartByKey(key types.NamespacedName, reason string, postBuild PostBuildHook) (*ManagedVM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.vms[key]
	if !ok {
		return nil, fmt.Errorf("registry: restart called for unknown key %s", key)
	}
	mi, err := r.build(key, old.source, old.SourceHash, old.limits, postBuild)
	if err != nil {
		return nil, err
	}
	old.VM.Close()
	mi.RestartCount = old.RestartCount + 1
	mi.LastReason = reason
	r.vms[key] = mi
	return mi, nil
}

// Len reports how many live VMs the registry is holding (test/metrics).
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.vms)
}
