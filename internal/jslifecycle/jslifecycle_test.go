package jslifecycle

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsregistry"
)

// captureEmitter buffers (eventType, reason, message) tuples so tests can
// assert what Rescue published.
func captureEmitter(buf int) (chan [3]string, EventEmitter) {
	ch := make(chan [3]string, buf)
	return ch, func(eventType, reason, message string) {
		ch <- [3]string{eventType, reason, message}
	}
}

// loadInstance parks a real qjs VM in reg under key so RestartByKey has
// something to rebuild from cached BuildOptions.
func loadInstance(t *testing.T, reg *jsregistry.Registry, key types.NamespacedName) {
	t.Helper()
	src := []byte(`function config(){return {configVersion:'v1'}} function handle(){}`)
	if _, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{Source: src, SourceHash: "h1"}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	t.Cleanup(func() { reg.Drop(jsregistry.HookKey(key)) })
}

// js-registry.R2
// status-conditions.R4
func TestRescue_Success_EmitsRestarted(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Namespace: "ns", Name: "ok"}
	loadInstance(t, reg, key)

	events, emit := captureEmitter(2)
	mi, err := Rescue(reg, jsregistry.HookKey(key), jsregistry.ReasonPanic, emit)
	if err != nil {
		t.Fatalf("Rescue: unexpected error: %v", err)
	}
	if mi == nil {
		t.Fatal("Rescue: ManagedVM is nil")
	}
	if last := mi.LastRestart(); last.Reason != jsregistry.ReasonPanic {
		t.Fatalf("Rescue: LastRestart.Reason=%q want %q", last.Reason, jsregistry.ReasonPanic)
	}

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
// status-conditions.R4
func TestRescue_Failure_EmitsRescueFailed(t *testing.T) {
	reg := jsregistry.NewRegistry()
	// Deliberately don't seed: RestartByKey on an unknown key returns an
	// error, which is exactly the path we want to assert publishes
	// RescueFailed (not Restarted).
	key := types.NamespacedName{Namespace: "ns", Name: "ghost"}

	events, emit := captureEmitter(2)
	if _, err := Rescue(reg, jsregistry.HookKey(key), jsregistry.ReasonMemoryLimit, emit); err == nil {
		t.Fatal("Rescue on unknown key: want error, got nil")
	}

	select {
	case ev := <-events:
		if ev[0] != corev1.EventTypeWarning {
			t.Errorf("eventType=%q want Warning", ev[0])
		}
		if ev[1] != conditions.EventRescueFailed {
			t.Errorf("reason=%q want %q", ev[1], conditions.EventRescueFailed)
		}
		if ev[2] != "rescue memory-limit failed" {
			t.Errorf("message=%q want stable template (no leaked underlying error)", ev[2])
		}
	default:
		t.Fatal("expected RescueFailed event, none captured")
	}
}

// js-registry.R2
// status-conditions.R4
func TestRescue_NilEmitter_NoOps(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Namespace: "ns", Name: "noemit"}
	loadInstance(t, reg, key)

	if _, err := Rescue(reg, jsregistry.HookKey(key), jsregistry.ReasonManual, nil); err != nil {
		t.Fatalf("Rescue with nil emitter must succeed: %v", err)
	}
}
