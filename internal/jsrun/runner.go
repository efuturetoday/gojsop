// Package jsrun is the port for script execution, in two interfaces. Runner
// is the data path: the dispatcher, the admission server and the Handle
// wrappers hand in a key, an export name and an input and get an output or a
// classified outcome. Scripts is the lifecycle path: the controllers ensure,
// drop and watch the scripts of their kind. Neither shows a VM, a lock or the
// engine.
//
// The first adapter is jsregistry.Registry: Ensure prepares a script once
// (compile, bake, snapshot) and every Invoke runs it statelessly on a fresh
// instance restored from that snapshot. Nothing survives between calls and
// nothing is recovered after one: a panic, a trap, a timeout or the memory
// limit just end that call, and the next call still gets a fresh instance
// from the same prepared script.
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
// loaded. The controllers use it to check that the entry point exists.
// Returning a non-nil error aborts the build.
//
// ctx is the build context, so the build deadline applies.
//
// js-registry.R10
type PostBuildHook func(ctx context.Context, s Script) error

// Spec bundles every input a runner needs to prepare (or prepare again) a
// script. The runner caches the whole struct, so it can recover a script
// without callers re-supplying source, limits or host binding.
//
// js-registry.R4
type Spec struct {
	Source     []byte
	SourceHash string
	Limits     Limits
	Host       Host
	PostBuild  PostBuildHook
	// ResetToken asks for a fresh script: when it differs from the token of
	// the prepared one, Ensure prepares again, the same path as a changed
	// source. The controller passes the value of the manual-restart
	// annotation, so editing the annotation resets the script.
	ResetToken string
	// Backoff spaces the retries of a Failed key. It does not shape the
	// script, so a change does not prepare again.
	Backoff Backoff
}

// RecoveryReason is the documented enum of causes that make a runner prepare a
// script again. Recorded into the status.instance counters of JSHook and
// JSAdmission (the CRD keeps its older field names, API-10).
type RecoveryReason string

// Recovery reasons, one per trigger of Ensure. The CRD enum of
// status.instance.recentRestarts[].reason lists exactly these
// (TestRecoveryReasons_MatchCRDEnum).
//   - ReasonSourceChanged: Spec.SourceHash differs
//   - ReasonLimitsChanged: Spec.Limits changed with the source hash unchanged
//   - ReasonManual:        Spec.ResetToken changed
const (
	ReasonSourceChanged RecoveryReason = "source-changed"
	ReasonLimitsChanged RecoveryReason = "limits-changed"
	ReasonManual        RecoveryReason = "manual"
)

// Recovery records one transition from an old script to a new one for a key.
type Recovery struct {
	Time   time.Time
	Reason RecoveryReason
	// Err carries the diagnostic for call-driven recoveries. Empty for
	// source-changed, limits-changed and manual.
	Err string
}

// Recoveries is the recovery log of a key: counts per reason and the recent
// entries. The adapter owns it and carries it across every prepared script.
type Recoveries struct {
	// ByReason aggregates Recent and older entries so callers don't recompute it.
	ByReason map[RecoveryReason]int32
	// Recent is the log (oldest first), capped by the adapter. Empty on the
	// very first preparation of a key.
	Recent []Recovery
}

// Last returns the most recent Recovery, or the zero value if the key has
// never been recovered (first preparation).
func (r Recoveries) Last() Recovery {
	if len(r.Recent) == 0 {
		return Recovery{}
	}
	return r.Recent[len(r.Recent)-1]
}

// Phase is the lifecycle phase of a runner key.
type Phase int

const (
	// PhasePreparing: a preparation runs; an older script may still serve
	// calls until it finishes. The zero value, so an empty State is not Ready.
	PhasePreparing Phase = iota
	// PhaseReady: a script matching the requested spec is prepared.
	PhaseReady
	// PhaseFailed: the last preparation failed. The entry holds no script.
	PhaseFailed
)

func (p Phase) String() string {
	switch p {
	case PhaseReady:
		return "Ready"
	case PhasePreparing:
		return "Preparing"
	default:
		return "Failed"
	}
}

// State is what Ensure reports for a key. It names no engine detail.
type State struct {
	Phase Phase
	// Err, Attempts and NextAttempt are set when Phase == PhaseFailed: the
	// last preparation error, how many failed in a row and the earliest time
	// Ensure starts the next try.
	Err         error
	Attempts    int
	NextAttempt time.Time
	// PreparedAt and Recoveries are set when Phase == PhaseReady.
	PreparedAt time.Time
	// SourceHash is the hash of the source the runner currently holds for the
	// key: the prepared script when Ready, the attempt in flight when
	// Preparing, the failed attempt when Failed. A caller that only reports
	// state (and does not Ensure) reads it instead of loading the source
	// again.
	SourceHash string
	Recoveries Recoveries
}

// minRetryIn keeps RetryIn from asking for a requeue in the past.
const minRetryIn = 10 * time.Millisecond

// RetryIn is how long a Failed key waits until Ensure starts the next try:
// the time left to NextAttempt, at least minRetryIn.
func (s State) RetryIn() time.Duration {
	return max(time.Until(s.NextAttempt), minRetryIn)
}

// Outcome classifies how an Invoke ran. Callers drive retry and failure
// policy from it.
type Outcome int

const (
	OutcomeOK          Outcome = iota // the export returned
	OutcomePanic                      // the call panicked (recovered)
	OutcomeMemoryLimit                // the script hit its memory limit
	OutcomeCancelled                  // the context ended: deadline or cancellation
	OutcomeError                      // any other error from the script
)

// Result is what Invoke hands back. Outcome drives the failure-policy
// decision; Duration is for metrics; Panic and Err carry the diagnostic
// detail. Nothing is recovered after a call, whatever the outcome: the
// instance Invoke ran on is thrown away either way, and the next Invoke gets
// a fresh one restored from the same prepared script.
type Result struct {
	Outcome  Outcome
	Duration time.Duration
	Panic    any   // populated when Outcome == OutcomePanic
	Err      error // populated when Outcome is neither OK nor Panic
}

// Runner is the data path: it runs a script. The dispatcher, the admission
// server and the typed Handle wrappers depend on this interface only.
//
// js-execution.R10
type Runner interface {
	// Invoke calls export of the script of key with in as its one argument
	// (none if in is nil) and decodes the JSON form of the result into out
	// (untouched when the export returns undefined; nil discards it). The
	// error is ErrUnknownKey or ErrVMUnavailable when nothing ran; otherwise
	// the Result classifies how the call ended. Every call runs in a fresh
	// script instance prepared by Ensure: no state survives between calls,
	// and nothing is recovered after a call, whatever the outcome. The
	// adapter bounds the call by the timeout of the Spec.
	Invoke(ctx context.Context, key Key, export string, in, out any) (Result, error)
}

// Scripts is the lifecycle path: controllers keep the scripts of their kind
// prepared. Every key is in one phase: Preparing, Ready or Failed.
//
// js-execution.R10
type Scripts interface {
	// Ensure reports the state of key for spec and never waits for a
	// preparation. Another spec (source hash, effective limits or ResetToken)
	// starts one; the key is notified on Watch when it ends.
	Ensure(key Key, spec Spec) State
	// Drop forgets key, cancels its preparation and releases its script.
	Drop(key Key)
	// State reports the state of key without starting a preparation, and
	// false when the key is unknown. A caller that only reports state — the
	// leader-only admission controller, which does not Ensure — reads it.
	State(key Key) (State, bool)
	// Watch delivers the keys of one kind whose preparation ended, until ctx
	// ends. Every call gets a stream of its own and every stream sees every
	// preparation, so a kind may have more than one reader.
	Watch(ctx context.Context, kind Kind) <-chan Key
}
