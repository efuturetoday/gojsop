package jshook_test

import (
	"context"
	"testing"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jshook"
)

func TestHandle_ReceivesBindingContext(t *testing.T) {
	const src = `
		var lastEvent = null;
		function handle(ctx) {
			lastEvent = ctx[0];
			return { ack: ctx.length };
		}
	`
	inst, err := jsengine.New(jsengine.Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)
	if err := inst.LoadModule(context.Background(), "hook.js", src); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}

	bctx := []jshook.BindingContext{{
		Binding:    "watch",
		Type:       "Event",
		WatchEvent: "Added",
		Object: map[string]any{
			"metadata": map[string]any{"name": "foo"},
		},
	}}
	got, err := jshook.Handle(context.Background(), inst, bctx)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := `{"ack":1}`
	if got != want {
		t.Fatalf("return: got %q want %q", got, want)
	}

	res, err := inst.Eval(context.Background(), "inspect.js", `lastEvent.watchEvent`)
	if err != nil {
		t.Fatalf("Eval inspect: %v", err)
	}
	if res != "Added" {
		t.Fatalf("lastEvent.watchEvent = %q", res)
	}
}

func TestHandle_MissingFunction(t *testing.T) {
	inst, err := jsengine.New(jsengine.Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(inst.Close)
	if err := inst.LoadModule(context.Background(), "nohandle.js", `function config(){return {}}`); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	if _, err := jshook.Handle(context.Background(), inst, nil); err == nil {
		t.Fatal("expected error when hook has no handle()")
	}
}
