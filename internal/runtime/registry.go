package runtime

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
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

// IsOOMError reports whether err comes from qjs hitting MemoryLimit. qjs surfaces
// it as a JS InternalError whose message contains "out of memory" (verified
// against fastschema/qjs v0.0.6 in TestMemoryLimit_Honoured).
func IsOOMError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "out of memory")
}

// ManagedInstance is the registry's view of a per-hook persistent runtime.
// Source/Resources are cached so the registry can rebuild the instance after
// a rescue restart without bouncing through the controller.
type ManagedInstance struct {
	Instance     *Instance
	SourceHash   string
	StartedAt    time.Time
	RestartCount int32
	LastReason   string
	// Config is the result of calling config() once at load time. It's cached
	// here so reconciles don't have to re-enter the JS runtime concurrently
	// with the dispatcher's handle() calls — qjs is not goroutine-safe and
	// the per-hook FIFO only serializes Handle, not LoadConfig.
	Config *Config

	// source and resources are the inputs needed to rebuild this instance after
	// a rescue restart (memory/panic/timeout/manual). They are not exported
	// because callers must go through Registry.RestartByKey to mutate them.
	source    []byte
	resources Resources
}

// Registry holds the live JS instance for every JSHook the controller knows
// about. One instance per NamespacedName, owned by the controller's lifetime.
//
// All methods are safe for concurrent use. The expectation is that the
// reconciler holds the per-hook lock implicitly via controller-runtime's
// per-key serialization, so contention here is rare.
type Registry struct {
	// Binder, if non-nil, is invoked on every newly created instance before
	// the user's module is evaluated. Use this to register host functions
	// (e.g. globalThis.kube) onto the runtime.
	Binder HostBinder

	mu        sync.Mutex
	instances map[types.NamespacedName]*ManagedInstance
}

func NewRegistry() *Registry {
	return &Registry{instances: make(map[types.NamespacedName]*ManagedInstance)}
}

// build constructs and initializes a fresh Instance for key with the given
// source and resources. On any failure the partially-built instance is closed.
// Caller holds r.mu.
func (r *Registry) build(key types.NamespacedName, source []byte, sourceHash string, res Resources) (*ManagedInstance, error) {
	inst, err := New(res)
	if err != nil {
		return nil, fmt.Errorf("registry: new instance: %w", err)
	}
	if err := inst.BindHost(r.Binder); err != nil {
		inst.Close()
		return nil, fmt.Errorf("registry: bind host: %w", err)
	}
	if err := inst.LoadModule(key.Name+".js", string(source)); err != nil {
		inst.Close()
		return nil, fmt.Errorf("registry: load module: %w", err)
	}
	cfg, err := inst.LoadConfig()
	if err != nil {
		inst.Close()
		return nil, fmt.Errorf("registry: load config: %w", err)
	}
	return &ManagedInstance{
		Instance:   inst,
		SourceHash: sourceHash,
		StartedAt:  time.Now(),
		Config:     cfg,
		source:     source,
		resources:  res,
	}, nil
}

// GetOrLoad returns the live ManagedInstance for key. If nothing exists yet,
// or if sourceHash differs from the last load, a fresh QuickJS runtime is
// started, the source is evaluated, and the previous instance (if any) is
// closed. The boolean reports whether a (re)start happened on this call.
func (r *Registry) GetOrLoad(key types.NamespacedName, source []byte, sourceHash string, res Resources) (*ManagedInstance, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.instances[key]
	if ok && existing.SourceHash == sourceHash {
		return existing, false, nil
	}

	mi, err := r.build(key, source, sourceHash, res)
	if err != nil {
		return nil, false, err
	}
	if ok {
		existing.Instance.Close()
		mi.RestartCount = existing.RestartCount + 1
		mi.LastReason = ReasonSourceChanged
	}
	r.instances[key] = mi
	return mi, true, nil
}

// Get returns the current ManagedInstance for key without modifying anything.
// The dispatcher's worker uses this to look up the live instance per dispatch,
// so a rescue restart transparently switches the next call to the new instance.
func (r *Registry) Get(key types.NamespacedName) (*ManagedInstance, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mi, ok := r.instances[key]
	return mi, ok
}

// Drop closes and forgets the instance for key. Idempotent; called from the
// reconciler when the JSHook resource is deleted.
func (r *Registry) Drop(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if mi, ok := r.instances[key]; ok {
		mi.Instance.Close()
		delete(r.instances, key)
	}
}

// RestartByKey force-replaces the instance for key with a fresh runtime, reusing
// the cached source/resources from the last successful load. Used by rescue
// paths (memory/panic/timeout/manual) that don't have the source bytes in hand.
// Returns an error if the hook is unknown to the registry.
func (r *Registry) RestartByKey(key types.NamespacedName, reason string) (*ManagedInstance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.instances[key]
	if !ok {
		return nil, fmt.Errorf("registry: restart called for unknown hook %s", key)
	}
	mi, err := r.build(key, old.source, old.SourceHash, old.resources)
	if err != nil {
		return nil, err
	}
	old.Instance.Close()
	mi.RestartCount = old.RestartCount + 1
	mi.LastReason = reason
	r.instances[key] = mi
	return mi, nil
}

// Len reports how many live instances the registry is holding (test/metrics).
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.instances)
}
