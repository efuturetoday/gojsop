package jshook_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/jshook"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsregistry/registrytest"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// loadHook loads src as the hook of a fresh key in a real registry.
func loadHook(t *testing.T, src string) (*jsregistry.Registry, jsrun.Key) {
	t.Helper()
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "hook"})
	t.Cleanup(func() { reg.Drop(key) })
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, jsrun.Spec{Source: []byte(src), SourceHash: "h"}); err != nil {
		t.Fatalf("load hook: %v", err)
	}
	return reg, key
}

// jshook.R7
func TestHandle_ReceivesBindingContext(t *testing.T) {
	const src = `
		function handle(ctx) {
			return { ack: ctx.length, watchEvent: ctx[0].watchEvent };
		}
	`
	reg, key := loadHook(t, src)

	bctx := []jshook.BindingContext{{
		Binding:    "watch",
		Type:       "Event",
		WatchEvent: "Added",
		Object: map[string]any{
			"metadata": map[string]any{"name": "foo"},
		},
	}}
	got, hres, err := jshook.Handle(context.Background(), reg, key, bctx)
	if err != nil || hres.Outcome != jsrun.OutcomeOK {
		t.Fatalf("Handle: %v, %+v", err, hres)
	}
	// The export returns both the ack and the watchEvent it saw in one shot:
	// every call restores a fresh instance from the snapshot, so nothing set
	// by this call could be inspected through a later, separate call.
	want := `{"ack":1,"watchEvent":"Added"}`
	if got != want {
		t.Fatalf("return: got %q want %q", got, want)
	}
}

func TestHandle_MissingFunction(t *testing.T) {
	reg, key := loadHook(t, `function config(){return {}}`)
	_, hres, err := jshook.Handle(context.Background(), reg, key, nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if hres.Err == nil {
		t.Fatal("expected error when hook has no handle()")
	}
}
