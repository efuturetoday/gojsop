// Package jslifecycle centralizes the bits of lifecycle plumbing that JSHook's
// dispatcher and JSAdmission's HTTP server share verbatim:
//
//   - the EventEmitter callback shape both packages use to publish corev1.Events
//     about their owning resource without depending on controller-runtime;
//   - the rescue helper that rebuilds a registered VM via the registry and
//     publishes the canonical Restarted / RescueFailed events with stable,
//     low-cardinality message templates.
//
// Putting this in one place collapses two parallel callback types and two
// near-identical rescue bodies into a single contract, so adding a third
// caller (or tweaking an event message) doesn't require touching both
// dispatcher and server.
package jslifecycle

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsregistry"
)

// EventEmitter publishes a corev1.Event about the JSHook or JSAdmission this
// callback is bound to. Optional throughout — every caller in this package
// accepts a nil emitter and silently no-ops.
//
// Reconcilers build the closure by binding a deepcopy of the resource to
// record.EventRecorder.Event so the recorder has a stable target across
// reconciles. The (eventType, reason, message) tuple is forwarded verbatim;
// message stability rules (see internal/conditions) are the caller's job.
type EventEmitter func(eventType, reason, message string)

// Rescue restarts the VM registered under key with the given reason and emits
// the canonical lifecycle event:
//
//   - on success: Warning Restarted, message "restarted: <reason>"
//   - on failure: Warning RescueFailed, message "rescue <reason> failed"
//
// Both message templates are drawn from the small set of registry restart
// reasons (jsregistry.ReasonPanic / ReasonMemoryLimit / ReasonTimeoutStreak /
// ReasonManual / ReasonSourceChanged), so the recorder's (Reason, Message)
// dedup window correctly collapses bursts of identical failures.
//
// The underlying error from RestartByKey is intentionally NOT folded into
// the event message — that string is wildly variable per-build and would
// defeat dedup. Callers should log it through their own logger.
//
// The emitter is optional; passing nil is supported.
//
// Block: js-registry R2
func Rescue(reg *jsregistry.Registry, key types.NamespacedName, reason jsregistry.RestartReason, emit EventEmitter) (*jsregistry.ManagedVM, error) {
	mi, err := reg.RestartByKey(key, reason)
	if err != nil {
		publish(emit, corev1.EventTypeWarning, conditions.EventRescueFailed,
			fmt.Sprintf("rescue %s failed", reason))
		return nil, err
	}
	publish(emit, corev1.EventTypeWarning, conditions.EventRestarted,
		fmt.Sprintf("restarted: %s", reason))
	return mi, nil
}

// RestartHistoryFor projects the registry's internal restart log onto the CRD
// status shape: RestartsByReason as string-keyed counters and RecentRestarts
// newest-first (so JSONPath print columns can read [0] without index-from-end
// gymnastics). The registry stores history oldest-first; we reverse here so
// the storage order stays the natural "append on transition" shape.
// Block: status-conditions R4
func RestartHistoryFor(mi *jsregistry.ManagedVM) (map[string]int32, []corev1alpha1.JSRestartEvent) {
	if mi == nil {
		return nil, nil
	}
	var byReason map[string]int32
	if len(mi.RestartsByReason) > 0 {
		byReason = make(map[string]int32, len(mi.RestartsByReason))
		for k, v := range mi.RestartsByReason {
			byReason[string(k)] = v
		}
	}
	var recent []corev1alpha1.JSRestartEvent
	if n := len(mi.History); n > 0 {
		recent = make([]corev1alpha1.JSRestartEvent, n)
		for i, ev := range mi.History {
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
