package jsregistry_test

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/efuturetoday/gojsop/internal/jsregistry"
	"github.com/efuturetoday/gojsop/internal/jsregistry/registrytest"
	"github.com/efuturetoday/gojsop/internal/jsrun"
)

// recvKey waits for one key on ch.
func recvKey(t *testing.T, ch <-chan jsrun.Key) jsrun.Key {
	t.Helper()
	select {
	case k, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed before a key arrived")
		}
		return k
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a build notification")
		return jsrun.Key{}
	}
}

// Two readers of one kind each see every finished build. The admission kind
// has two of them: the leader-only status controller and the server
// controller that runs on every replica.
//
// js-registry.R21
func TestRegistry_Watch_FansOutToEverySubscriber(t *testing.T) {
	reg := jsregistry.NewRegistry()
	ctx := t.Context()

	first := reg.Watch(ctx, jsrun.KindJSAdmission)
	second := reg.Watch(ctx, jsrun.KindJSAdmission)

	key := jsrun.AdmissionKey(types.NamespacedName{Name: "p1"})
	t.Cleanup(func() { reg.Drop(key) })
	reg.Ensure(key, jsrun.Spec{Source: []byte(`function validate(){return{allowed:true}}`), SourceHash: "h1"})

	if got := recvKey(t, first); got != key {
		t.Errorf("first subscriber: got %v, want %v", got, key)
	}
	if got := recvKey(t, second); got != key {
		t.Errorf("second subscriber: got %v, want %v", got, key)
	}
}

// A build that ends before a reader exists is not lost: the reader inherits
// the backlog of its kind when it registers.
//
// js-registry.R18
func TestRegistry_Watch_LateSubscriberInheritsFinishedBuilds(t *testing.T) {
	reg := jsregistry.NewRegistry()
	ctx := t.Context()

	key := jsrun.HookKey(types.NamespacedName{Name: "h1"})
	t.Cleanup(func() { reg.Drop(key) })

	buildCtx, buildCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer buildCancel()
	if _, _, err := registrytest.GetOrLoad(reg, buildCtx, key,
		jsrun.Spec{Source: []byte(`function handle(){}`), SourceHash: "h1"}); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}

	// Only now does anybody watch.
	if got := recvKey(t, reg.Watch(ctx, jsrun.KindJSHook)); got != key {
		t.Errorf("late subscriber: got %v, want %v", got, key)
	}
}

// A dropped key leaves the backlog, so a reader that registers afterwards is
// not told to reconcile a resource that is gone.
//
// js-registry.R18
func TestRegistry_Watch_DroppedKeyLeavesTheBacklog(t *testing.T) {
	reg := jsregistry.NewRegistry()
	ctx := t.Context()

	key := jsrun.HookKey(types.NamespacedName{Name: "h1"})
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer buildCancel()
	if _, _, err := registrytest.GetOrLoad(reg, buildCtx, key,
		jsrun.Spec{Source: []byte(`function handle(){}`), SourceHash: "h1"}); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	reg.Drop(key)

	ch := reg.Watch(ctx, jsrun.KindJSHook)
	select {
	case k, ok := <-ch:
		if ok {
			t.Errorf("got notification for dropped key %v", k)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

// A subscriber that goes away with its context frees its slot, so a later
// build does not pile up notifications nobody reads.
//
// js-registry.R21
func TestRegistry_Watch_SubscriptionEndsWithTheContext(t *testing.T) {
	reg := jsregistry.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	ch := reg.Watch(ctx, jsrun.KindJSHook)
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected the channel to close, got a key")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch channel did not close after the context ended")
	}

	// The registry keeps working for the readers that are left.
	next := reg.Watch(t.Context(), jsrun.KindJSHook)
	key := jsrun.HookKey(types.NamespacedName{Name: "h2"})
	t.Cleanup(func() { reg.Drop(key) })
	reg.Ensure(key, jsrun.Spec{Source: []byte(`function handle(){}`), SourceHash: "h2"})
	if got := recvKey(t, next); got != key {
		t.Errorf("got %v, want %v", got, key)
	}
}

// State carries the source hash the runner holds, in every phase. A caller
// that only reports state reads it instead of loading the source again.
//
// js-registry.R22
func TestRegistry_Ensure_StateCarriesSourceHash(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "h1"})
	t.Cleanup(func() { reg.Drop(key) })

	preparing := reg.Ensure(key, jsrun.Spec{Source: []byte(`function handle(){}`), SourceHash: "hash-ok"})
	if preparing.Phase != jsrun.PhasePreparing {
		t.Fatalf("Phase: got %v, want Preparing", preparing.Phase)
	}
	if preparing.SourceHash != "hash-ok" {
		t.Errorf("Preparing SourceHash: got %q, want %q", preparing.SourceHash, "hash-ok")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := registrytest.GetOrLoad(reg, ctx, key,
		jsrun.Spec{Source: []byte(`function handle(){}`), SourceHash: "hash-ok"}); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if got := reg.Ensure(key, jsrun.Spec{Source: []byte(`function handle(){}`), SourceHash: "hash-ok"}); got.Phase != jsrun.PhaseReady || got.SourceHash != "hash-ok" {
		t.Errorf("Ready state: got phase %v hash %q, want Ready hash-ok", got.Phase, got.SourceHash)
	}

	broken := jsrun.Spec{Source: []byte(`syntax ( error`), SourceHash: "hash-bad"}
	failCtx, failCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer failCancel()
	if _, _, err := registrytest.GetOrLoad(reg, failCtx, key, broken); err == nil {
		t.Fatal("expected the broken source to fail the build")
	}
	if got := reg.Ensure(key, broken); got.Phase != jsrun.PhaseFailed || got.SourceHash != "hash-bad" {
		t.Errorf("Failed state: got phase %v hash %q, want Failed hash-bad", got.Phase, got.SourceHash)
	}
}
