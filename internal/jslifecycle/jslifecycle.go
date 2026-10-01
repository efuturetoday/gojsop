// Package jslifecycle centralizes the bits of lifecycle plumbing that JSHook's
// dispatcher and JSAdmission's HTTP server share verbatim:
//
//   - the EventEmitter callback shape both packages use to publish corev1.Events
//     about their owning resource without depending on controller-runtime;
//   - the helper that announces a recovery of a script (which the runner
//     starts itself inside Invoke) as the canonical Restarted event with a
//     stable, low-cardinality message template.
//
// Putting this in one place collapses two parallel callback types and two
// near-identical bodies into a single contract, so adding a third
// caller (or tweaking an event message) doesn't require touching both
// dispatcher and server.
package jslifecycle

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// EventEmitter publishes a corev1.Event about the JSHook or JSAdmission this
// callback is bound to. Optional throughout — every caller in this package
// accepts a nil emitter and silently no-ops.
//
// Reconcilers build the closure by binding a deepcopy of the resource to
// events.EventRecorder.Eventf so the recorder has a stable target across
// reconciles. The (eventType, reason, message) tuple is forwarded verbatim;
// message stability rules (see internal/conditions) are the caller's job.
type EventEmitter func(eventType, reason, message string)

// Announce publishes the canonical lifecycle event for a recovery that the
// runner started inside Invoke (Result.Recovered): Warning Restarted, message
// "restarted: <reason>". It does nothing for an empty reason (the call needed
// no recovery). The runner recovers the script itself; callers only announce.
//
// The message template is drawn from the small set of recovery reasons
// (jsrun.ReasonPanic / ReasonMemoryLimit / ReasonTimeout), so the recorder's
// (Reason, Message) dedup window collapses bursts of identical failures.
//
// The emitter is optional; passing nil is supported.
//
// js-registry.R2
func Announce(emit EventEmitter, reason jsrun.RecoveryReason) {
	if reason == "" {
		return
	}
	publish(emit, corev1.EventTypeWarning, conditions.EventRestarted,
		fmt.Sprintf("restarted: %s", reason))
}

// RestartHistoryFor projects the runner's recovery log onto the CRD
// status shape: RestartsByReason as string-keyed counters and RecentRestarts
// newest-first (so JSONPath print columns can read [0] without index-from-end
// gymnastics). The runner stores the log oldest-first; we reverse here so
// the storage order stays the natural "append on transition" shape. The CRD
// keeps its older field names (API-10).
// status-conditions.R4
func RestartHistoryFor(rec jsrun.Recoveries) (map[string]int32, []corev1alpha1.JSRestartEvent) {
	var byReason map[string]int32
	if len(rec.ByReason) > 0 {
		byReason = make(map[string]int32, len(rec.ByReason))
		for k, v := range rec.ByReason {
			byReason[string(k)] = v
		}
	}
	var recent []corev1alpha1.JSRestartEvent
	if n := len(rec.Recent); n > 0 {
		recent = make([]corev1alpha1.JSRestartEvent, n)
		for i, ev := range rec.Recent {
			t := metav1.NewTime(ev.Time)
			recent[n-1-i] = corev1alpha1.JSRestartEvent{
				Time:   &t,
				Reason: string(ev.Reason),
				Error:  ev.Err,
			}
		}
	}
	return byReason, recent
}

func publish(emit EventEmitter, eventType, reason, message string) {
	if emit != nil {
		emit(eventType, reason, message)
	}
}
