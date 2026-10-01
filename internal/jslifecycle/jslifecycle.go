// Package jslifecycle centralizes the bits of lifecycle plumbing that JSHook's
// dispatcher and JSAdmission's HTTP server share verbatim: the EventEmitter
// callback shape both packages use to publish corev1.Events about their
// owning resource without depending on controller-runtime, the projection
// of the runner's recovery log onto the CRD status shape (RestartHistoryFor),
// and status.lastReconcile after a success (ReconcileSucceeded).
//
// A runner's Invoke never recovers a script itself: a panic,
// a trap, a timeout or the memory limit just end that call, and the next call
// gets a fresh instance from the same prepared script. Only Ensure prepares a
// script again (source changed, limits changed, manual reset), and that is
// reported through the Scripts.Ensure state, not through this package.
package jslifecycle

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
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

// ReconcileSucceeded returns status.lastReconcile after a successful
// reconcile: the error of an earlier failure is cleared and the time set to
// now; a status that already reports success is returned unchanged, so a
// steady reconcile does not write status again and trigger itself.
// status-conditions.R5
func ReconcileSucceeded(prev *corev1alpha1.JSReconcileStatus, now time.Time) *corev1alpha1.JSReconcileStatus {
	if prev != nil && prev.Time != nil && prev.Error == "" {
		return prev
	}
	t := metav1.NewTime(now)
	return &corev1alpha1.JSReconcileStatus{Time: &t}
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
