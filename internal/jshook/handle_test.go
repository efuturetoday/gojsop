package jshook_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/efuturetoday/gojsop/internal/jsengine/kubehost"
	"github.com/efuturetoday/gojsop/internal/jshook"
	"github.com/efuturetoday/gojsop/internal/jsregistry"
	"github.com/efuturetoday/gojsop/internal/jsregistry/registrytest"
	"github.com/efuturetoday/gojsop/internal/jsrun"
)

// loadHook loads src as the hook of a fresh key in a real registry.
func loadHook(t *testing.T, src string) (*jsregistry.Registry, jsrun.Key) {
	t.Helper()
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "hook"})
	t.Cleanup(func() { reg.Drop(key) })
	spec := jsrun.Spec{Source: []byte(src), SourceHash: "h", Host: kubehost.HookEvents{}}
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, spec); err != nil {
		t.Fatalf("load hook: %v", err)
	}
	return reg, key
}

// jshook.R7
func TestHandle_ReceivesOneEvent(t *testing.T) {
	const src = `
		function handle(event) {
			return { type: event.type, binding: event.binding, name: event.object.metadata.name, initial: event.initial };
		}
	`
	reg, key := loadHook(t, src)
	ev := jshook.Event{
		Binding: "watch",
		Type:    "Added",
		Object:  map[string]any{"metadata": map[string]any{"name": "foo"}},
		Initial: true,
	}
	got, hres, err := jshook.Handle(context.Background(), reg, key, ev)
	if err != nil || hres.Outcome != jsrun.OutcomeOK {
		t.Fatalf("Handle: %v, %+v", err, hres)
	}
	want := `{"type":"Added","binding":"watch","name":"foo","initial":true}`
	if got != want {
		t.Fatalf("return: got %q want %q", got, want)
	}
}

// event.all() answers from the Lister of the call; without one it throws.
// jshook.R26
func TestHandle_AllAnswersFromTheListerOfTheCall(t *testing.T) {
	reg, key := loadHook(t, `function handle(e) { return e.all().map(function (o) { return o.metadata.name; }); }`)
	ev := jshook.Event{Binding: "watch", Type: "Modified", Object: map[string]any{}}

	ctx := jshook.WithLister(context.Background(), func() ([]map[string]any, error) {
		return []map[string]any{{"metadata": map[string]any{"name": "a"}}, {"metadata": map[string]any{"name": "b"}}}, nil
	})
	got, hres, err := jshook.Handle(ctx, reg, key, ev)
	if err != nil || hres.Outcome != jsrun.OutcomeOK {
		t.Fatalf("Handle: %v, %+v", err, hres)
	}
	if got != `["a","b"]` {
		t.Fatalf("all(): got %s", got)
	}

	_, hres, err = jshook.Handle(context.Background(), reg, key, ev)
	if err != nil || hres.Outcome != jsrun.OutcomeError || !strings.Contains(hres.Err.Error(), "event.all()") {
		t.Fatalf("all() without a lister: %v, %+v", err, hres)
	}
	// The error names the author's function, not the wrapper around it.
	if !strings.Contains(hres.Err.Error(), "calling handle()") {
		t.Fatalf("error %q does not name handle()", hres.Err)
	}
}
