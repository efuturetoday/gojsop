package jslifecycle

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// captureEmitter buffers (eventType, reason, message) tuples so tests can
// assert what Announce published.
func captureEmitter(buf int) (chan [3]string, EventEmitter) {
	ch := make(chan [3]string, buf)
	return ch, func(eventType, reason, message string) {
		ch <- [3]string{eventType, reason, message}
	}
}

// js-registry.R2
// status-conditions.R4
func TestAnnounce_Recovery_EmitsRestarted(t *testing.T) {
	events, emit := captureEmitter(2)
	Announce(emit, jsrun.ReasonPanic)

	select {
	case ev := <-events:
		if ev[0] != corev1.EventTypeWarning {
			t.Errorf("eventType=%q want Warning", ev[0])
		}
		if ev[1] != conditions.EventRestarted {
			t.Errorf("reason=%q want %q", ev[1], conditions.EventRestarted)
		}
		if ev[2] != "restarted: panic" {
			t.Errorf("message=%q want %q (stable template, finite reason set drives recorder dedup)", ev[2], "restarted: panic")
		}
	default:
		t.Fatal("expected Restarted event, none captured")
	}
}

// js-registry.R2
func TestAnnounce_NoRecovery_PublishesNothing(t *testing.T) {
	events, emit := captureEmitter(2)
	Announce(emit, "")
	select {
	case ev := <-events:
		t.Fatalf("no recovery, but event published: %v", ev)
	default:
	}
}

// js-registry.R2
func TestAnnounce_NilEmitter_NoOps(t *testing.T) {
	Announce(nil, jsrun.ReasonManual)
}
