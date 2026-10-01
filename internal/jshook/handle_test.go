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
func loadHook(t *testing.T, src string) (*jsregistry.Registry, jsrun.Key, *jsregistry.ManagedVM) {
	t.Helper()
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "hook"})
	t.Cleanup(func() { reg.Drop(key) })
	mi, _, err := registrytest.GetOrLoad(reg, context.Background(), key, jsrun.Options{Source: []byte(src), SourceHash: "h"})
	if err != nil {
		t.Fatalf("load hook: %v", err)
	}
	return reg, key, mi
}

// jshook.R7
func TestHandle_ReceivesBindingContext(t *testing.T) {
	const src = `
		var lastEvent = null;
		function handle(ctx) {
			lastEvent = ctx[0];
			return { ack: ctx.length };
		}
	`
	reg, key, mi := loadHook(t, src)

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
	want := `{"ack":1}`
	if got != want {
		t.Fatalf("return: got %q want %q", got, want)
	}

	res, err := mi.VM.Eval(context.Background(), "inspect.js", `lastEvent.watchEvent`)
	if err != nil {
		t.Fatalf("Eval inspect: %v", err)
	}
	if res != "Added" {
		t.Fatalf("lastEvent.watchEvent = %q", res)
	}
}

func TestHandle_MissingFunction(t *testing.T) {
	reg, key, _ := loadHook(t, `function config(){return {}}`)
	_, hres, err := jshook.Handle(context.Background(), reg, key, nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if hres.Err == nil {
		t.Fatal("expected error when hook has no handle()")
	}
}
