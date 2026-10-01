// Package jsrun is the port for script execution. The dispatcher, the
// admission server, the controllers and jslifecycle talk to a Runner: they
// hand in a key, an export name and an input and get an output or a
// classified outcome. They never see a VM, a lock or the engine.
//
// The first adapter is jsregistry.Registry (one persistent VM per key). The
// port also fits an engine that prepares a script once (compile, bake,
// snapshot) and invokes it statelessly: Ensure is "prepare", Invoke is "run".
//
// js-execution.R10
package jsrun

import (
	"context"
	"time"
)

// Limits caps what a single script may consume. Zero fields fall back to the
// defaults in DefaultLimits(); callers plumb explicit values from spec.limits
// on the JSHook/JSAdmission CRDs.
type Limits struct {
	// MemoryMB caps the script heap in megabytes. Zero means "use default".
	MemoryMB int32
	// TimeoutSeconds bounds a single call and a build. The adapter enforces
	// it through the context of the call. Zero means "use default".
	TimeoutSeconds int32
}

// DefaultLimits matches the CRD doc defaults (memoryMB:32, timeoutSeconds:30).
func DefaultLimits() Limits {
	return Limits{MemoryMB: 32, TimeoutSeconds: 30}
}

// WithDefaults returns l with zero fields replaced by DefaultLimits. Callers
// compare or read limits through it so "unset" and "explicit default" agree.
func (l Limits) WithDefaults() Limits {
	d := DefaultLimits()
	if l.MemoryMB <= 0 {
		l.MemoryMB = d.MemoryMB
	}
	if l.TimeoutSeconds <= 0 {
		l.TimeoutSeconds = d.TimeoutSeconds
	}
	return l
}

// Host is an engine-specific binding of host APIs (the `kube` object). It is
// opaque to callers: they take it from kubehost.Factory and pass it on in
// Options. An adapter rejects a value it cannot bind with ErrBindHost.
type Host any

// Script is what a PostBuildHook sees of a freshly built script: whether it
// exports a function and a call of one. The build deadline applies.
type Script interface {
	// HasExport reports whether the script exports a callable named name.
	HasExport(name string) bool
	// Invoke calls the export with in as its one argument (none if in is
	// nil) and decodes the JSON form of the result into out. out stays
	// untouched when the export returns undefined; a nil out discards it.
	Invoke(ctx context.Context, export string, in, out any) error
}

// PostBuildHook is invoked once per fresh script after the source has
// loaded. It returns the value to stash on Instance.Extra. JSHook uses it to
// call jshook.ReadConfig and cache the parsed Config. Returning a non-nil
// error aborts the build.
//
// ctx is the build context: config() runs once per build, so the build
// deadline applies.
//
// js-registry.R10
type PostBuildHook func(ctx context.Context, s Script) (extra any, err error)

// Options bundles every input a runner needs to build (or rebuild) a script.
// The runner caches the whole struct, so Restart can rebuild without callers
// re-supplying source, limits or host binding.
//
// js-registry.R4
type Options struct {
	Source     []byte
	SourceHash string
	Limits     Limits
	Host       Host
	PostBuild  PostBuildHook
	// Backoff spaces the retries of a Broken key. It does not shape the
	// script, so a change does not rebuild.
	Backoff Backoff
}

// RestartReason is the documented enum of triggers that cause a script to be
// rebuilt. Recorded into JSHook.status.instance.lastRestartReason.
type RestartReason string

// Restart reasons. Every value is wired to a real trigger:
//   - ReasonSourceChanged: spec.source's hash differs on Reconcile (Ensure)
//   - ReasonMemoryLimit:   handle() returned an "out of memory" error (dispatcher worker / admission)
//   - ReasonPanic:         handle() panicked (recovered by Invoke)
//   - ReasonTimeout:       handle() exceeded the per-call deadline (admission, dispatcher)
//   - ReasonTimeoutStreak: no longer set: a cancelled call closes the module, so every timeout restarts with ReasonTimeout; kept as CRD enum value
//   - ReasonManual:        gojsop.io/restart annotation changed on the JSHook (reconciler)
//   - ReasonLimitsChanged: spec.limits changed with the source hash unchanged (Ensure)
const (
	ReasonSourceChanged RestartReason = "source-changed"
	ReasonMemoryLimit   RestartReason = "memory-limit"
	ReasonPanic         RestartReason = "panic"
	ReasonTimeout       RestartReason = "timeout"
	ReasonTimeoutStreak RestartReason = "timeout-streak"
	ReasonManual        RestartReason = "manual"
	ReasonLimitsChanged RestartReason = "limits-changed"
)

// ReportedByReconcile reports whether a controller reports the restart with
// this reason as a Restarted event after the build. Restarts that a call
// started (panic, memory limit, timeout) are reported by jslifecycle.Rescue
// when they start.
func (r RestartReason) ReportedByReconcile() bool {
	switch r {
	case ReasonSourceChanged, ReasonLimitsChanged:
		return true
	default:
		return false
	}
}

// RestartEvent records one transition from an old script to a new one for a
// key. The slice of these on Instance is the authoritative restart log;
// per-reason counters are an aggregate cache.
type RestartEvent struct {
	Time   time.Time
	Reason RestartReason
	// Err carries the diagnostic for rescue-driven restarts (panic / OOM /
	// timeout / timeout-streak). Empty for source-changed and manual.
	Err string
}

// Instance is what a caller may know of a ready script: when it started, the
// effective limits, the restart log and the value of the PostBuildHook.
type Instance struct {
	StartedAt time.Time
	// Limits are the effective limits (defaults applied).
	Limits Limits
	// RestartsByReason aggregates History so callers don't recompute it.
	RestartsByReason map[RestartReason]int32
	// History is the restart log (oldest first), capped by the adapter.
	// Empty on the very first build of a key.
	History []RestartEvent
	// Extra is the value the PostBuildHook returned (e.g. JSHook caches its
	// parsed Config here so reconciles never enter the script).
	Extra any
}

// LastRestart returns the most recent RestartEvent, or the zero value if the
// script has never been restarted (first build).
func (i *Instance) LastRestart() RestartEvent {
	if len(i.History) == 0 {
		return RestartEvent{}
	}
	return i.History[len(i.History)-1]
}

// StateKind is the lifecycle state of a runner key.
type StateKind int

const (
	// StateReady: a script matching the requested options is installed.
	StateReady StateKind = iota
	// StateBuilding: a build runs; an older script may still serve calls until
	// it finishes.
	StateBuilding
	// StateBroken: the last build failed. The entry holds no script.
	StateBroken
)

func (k StateKind) String() string {
	switch k {
	case StateReady:
		return "Ready"
	case StateBuilding:
		return "Building"
	default:
		return "Broken"
	}
}

// State is what Ensure reports for a key.
type State struct {
	Kind StateKind
	// Instance is set when Kind == StateReady.
	Instance *Instance
	// Err, Attempts and NextTry are set when Kind == StateBroken: the last
	// build error, how many builds failed in a row and the earliest time Ensure
	// starts the next try.
	Err      error
	Attempts int
	NextTry  time.Time
}

// minRetryIn keeps RetryIn from asking for a requeue in the past.
const minRetryIn = 10 * time.Millisecond

// RetryIn is how long a Broken key waits until Ensure starts the next build:
// the time left to NextTry, at least minRetryIn.
func (s State) RetryIn() time.Duration {
	return max(time.Until(s.NextTry), minRetryIn)
}

// Outcome classifies how an Invoke ran. Callers drive rescue and failure
// policy from it.
type Outcome int

const (
	OutcomeOK          Outcome = iota // the export returned
	OutcomePanic                      // the call panicked (recovered)
	OutcomeMemoryLimit                // the script hit its memory limit
	OutcomeCancelled                  // the context ended: deadline or cancellation
	OutcomeError                      // any other error from the script
)

// Result is what Invoke hands back. Outcome drives the rescue and
// failure-policy decision; Duration is for metrics; Panic and Err carry the
// diagnostic detail.
type Result struct {
	Outcome  Outcome
	Duration time.Duration
	Panic    any   // populated when Outcome == OutcomePanic
	Err      error // populated when Outcome is neither OK nor Panic
}

// Runner runs the scripts of JSHooks and JSAdmissions. Every key is in one
// state: Ready, Building or Broken.
//
// js-execution.R10
type Runner interface {
	// Ensure reports the state of key for opts and never waits for a build.
	// Other opts than the installed ones start a build; the key is notified
	// on Watch when it ends.
	Ensure(key Key, opts Options) State
	// Invoke calls export of the script of key with in as its one argument
	// (none if in is nil) and decodes the JSON form of the result into out
	// (untouched when the export returns undefined; nil discards it). The
	// error is ErrUnknownKey or ErrVMUnavailable when nothing ran; otherwise
	// the Result classifies how the call ended.
	Invoke(ctx context.Context, key Key, export string, in, out any) (Result, error)
	// Restart drops the script of key and rebuilds it from the cached
	// options, without waiting. ErrUnknownKey if the key is not known.
	Restart(key Key, reason RestartReason) error
	// Drop forgets key, cancels its build and releases its script.
	Drop(key Key)
	// Instance returns the ready script of key, if any.
	Instance(key Key) (*Instance, bool)
	// Known reports whether key is held in any state. With Instance it tells
	// "not ready yet" (retry later) from "never registered or dropped".
	Known(key Key) bool
	// Watch delivers the keys of one kind whose build ended, until ctx ends.
	Watch(ctx context.Context, kind Kind) <-chan Key
}
