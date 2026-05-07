package runtime

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Restart reasons recorded into JSHook.status.instance.lastRestartReason.
const (
	ReasonSourceChanged = "source-changed"
	ReasonMemoryLimit   = "memory-limit"
	ReasonPanic         = "panic"
	ReasonTimeoutStreak = "timeout-streak"
	ReasonManual        = "manual"
)

// ManagedInstance is the registry's view of a per-hook persistent runtime.
// The Instance survives across reconciles; SourceHash drives restart-on-change.
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

	inst, err := New(res)
	if err != nil {
		return nil, false, fmt.Errorf("registry: new instance: %w", err)
	}
	if err := inst.BindHost(r.Binder); err != nil {
		inst.Close()
		return nil, false, fmt.Errorf("registry: bind host: %w", err)
	}
	if err := inst.LoadModule(key.Name+".js", string(source)); err != nil {
		inst.Close()
		return nil, false, fmt.Errorf("registry: load module: %w", err)
	}
	cfg, err := inst.LoadConfig()
	if err != nil {
		inst.Close()
		return nil, false, fmt.Errorf("registry: load config: %w", err)
	}

	mi := &ManagedInstance{
		Instance:   inst,
		SourceHash: sourceHash,
		StartedAt:  time.Now(),
		Config:     cfg,
	}
	if ok {
		// restart due to source change
		existing.Instance.Close()
		mi.RestartCount = existing.RestartCount + 1
		mi.LastReason = ReasonSourceChanged
	}
	r.instances[key] = mi
	return mi, true, nil
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

// Restart force-replaces the instance for key with a fresh runtime, recording
// reason. Used when the persistent instance is unhealthy (panic, OOM, repeated
// timeouts). Returns the new ManagedInstance or an error if the source isn't
// known yet.
func (r *Registry) Restart(key types.NamespacedName, source []byte, sourceHash, reason string, res Resources) (*ManagedInstance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.instances[key]
	if !ok {
		return nil, fmt.Errorf("registry: restart called for unknown hook %s", key)
	}
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
	old.Instance.Close()
	mi := &ManagedInstance{
		Instance:     inst,
		SourceHash:   sourceHash,
		StartedAt:    time.Now(),
		RestartCount: old.RestartCount + 1,
		LastReason:   reason,
		Config:       cfg,
	}
	r.instances[key] = mi
	return mi, nil
}

// Len reports how many live instances the registry is holding (test/metrics).
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.instances)
}
