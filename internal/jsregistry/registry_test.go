package jsregistry_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsregistry"
)

// js-registry.R5
func TestRegistry_PersistsAcrossLoads(t *testing.T) {
	reg := jsregistry.NewRegistry()
	t.Cleanup(func() { reg.Drop(jsregistry.HookKey(types.NamespacedName{Name: "h1"})) })

	src := []byte(`globalThis.counter = (globalThis.counter || 0); function config(){return {configVersion:"v1"}}`)
	key := types.NamespacedName{Name: "h1"}
	opts := jsregistry.BuildOptions{Source: src, SourceHash: "abc123"}

	mi, restarted, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), opts)
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if !restarted {
		t.Fatal("expected restarted=true on first load")
	}
	if got := len(mi.History); got != 0 {
		t.Errorf("History length: got %d, want 0", got)
	}

	if _, err := mi.VM.Eval(context.Background(), "inc.js", "globalThis.counter = 42"); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	mi2, restarted2, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), opts)
	if err != nil {
		t.Fatalf("GetOrLoad #2: %v", err)
	}
	if restarted2 {
		t.Fatal("expected restarted=false when hash unchanged")
	}
	if mi2 != mi {
		t.Fatal("expected identical ManagedVM pointer")
	}
	got, err := mi2.VM.Eval(context.Background(), "read.js", "globalThis.counter")
	if err != nil {
		t.Fatalf("Eval read: %v", err)
	}
	if got != "42" {
		t.Fatalf("state lost: counter = %q (want 42)", got)
	}
}

// js-registry.R3
// js-sources.R3
func TestRegistry_RestartOnSourceChange(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "h2"}
	t.Cleanup(func() { reg.Drop(jsregistry.HookKey(key)) })

	src1 := []byte(`globalThis.tag = "v1"; function config(){return {}}`)
	src2 := []byte(`globalThis.tag = "v2"; function config(){return {}}`)

	mi, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{Source: src1, SourceHash: "h1"})
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	got, _ := mi.VM.Eval(context.Background(), "t.js", "globalThis.tag")
	if got != "v1" {
		t.Fatalf("v1 tag: got %q", got)
	}

	mi2, restarted, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{Source: src2, SourceHash: "h2"})
	if err != nil {
		t.Fatalf("load v2: %v", err)
	}
	if !restarted {
		t.Fatal("expected restarted=true on source change")
	}
	if got := len(mi2.History); got != 1 {
		t.Errorf("History length: got %d, want 1", got)
	}
	if last := mi2.LastRestart(); last.Reason != jsregistry.ReasonSourceChanged {
		t.Errorf("LastRestart.Reason: got %q", last.Reason)
	}
	if got := mi2.RestartsByReason[jsregistry.ReasonSourceChanged]; got != 1 {
		t.Errorf("RestartsByReason[source-changed]: got %d, want 1", got)
	}
	got2, _ := mi2.VM.Eval(context.Background(), "t.js", "globalThis.tag")
	if got2 != "v2" {
		t.Fatalf("v2 tag: got %q", got2)
	}
}

// TestRegistry_RestartByKey_RebuildsFromCachedSource proves that the registry
// can rescue-rebuild an instance using the cached BuildOptions, without the
// caller passing them again. This is the path the dispatcher takes on
// memory/panic/timeout — it doesn't have the source bytes in hand.
// js-registry.R4
func TestRegistry_RestartByKey_RebuildsFromCachedSource(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "rescue"}
	t.Cleanup(func() { reg.Drop(jsregistry.HookKey(key)) })

	src := []byte(`globalThis.counter = 0; function config(){return {}}`)
	mi, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{
		Source:     src,
		SourceHash: "h1",
		Limits:     jsengine.Limits{MemoryMB: 8},
	})
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if _, err := mi.VM.Eval(context.Background(), "dirty.js", "globalThis.counter = 99"); err != nil {
		t.Fatalf("dirty eval: %v", err)
	}

	mi2, err := reg.RestartByKey(jsregistry.HookKey(key), jsregistry.ReasonPanic)
	if err != nil {
		t.Fatalf("RestartByKey: %v", err)
	}
	if mi2 == mi {
		t.Fatal("RestartByKey must return a fresh ManagedVM pointer")
	}
	if last := mi2.LastRestart(); last.Reason != jsregistry.ReasonPanic {
		t.Errorf("LastRestart.Reason: got %q, want %q", last.Reason, jsregistry.ReasonPanic)
	}
	if got := len(mi2.History); got != 1 {
		t.Errorf("History length: got %d, want 1", got)
	}
	if got := mi2.RestartsByReason[jsregistry.ReasonPanic]; got != 1 {
		t.Errorf("RestartsByReason[panic]: got %d, want 1", got)
	}
	if mi2.VM.Limits().MemoryMB != 8 {
		t.Errorf("limits not preserved: got MemoryMB=%d", mi2.VM.Limits().MemoryMB)
	}
	got, err := mi2.VM.Eval(context.Background(), "read.js", "globalThis.counter")
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if got != "0" {
		t.Fatalf("globalThis.counter survived rescue: got %q (want 0)", got)
	}
}

// status-conditions.R4
// TestRegistry_RestartHistory_RingAndCounters proves the per-reason counter
// increments correctly across many restarts and that History is capped at
// historyCap (20) with the oldest event evicted on overflow.
// js-registry.R11
func TestRegistry_RestartHistory_RingAndCounters(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "ring"}
	t.Cleanup(func() { reg.Drop(jsregistry.HookKey(key)) })

	src := []byte(`function config(){return {}}`)
	if _, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{Source: src, SourceHash: "x"}); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	// Three manual restarts → counter == 3, history == 3.
	for i := range 3 {
		if _, err := reg.RestartByKey(jsregistry.HookKey(key), jsregistry.ReasonManual); err != nil {
			t.Fatalf("restart #%d: %v", i, err)
		}
	}
	mi, _ := reg.Get(jsregistry.HookKey(key))
	if got := mi.RestartsByReason[jsregistry.ReasonManual]; got != 3 {
		t.Errorf("RestartsByReason[manual] after 3: got %d, want 3", got)
	}
	if got := len(mi.History); got != 3 {
		t.Errorf("History length after 3: got %d, want 3", got)
	}

	// Push another 22 (total 25) — ring should cap at 20, oldest evicted,
	// counter keeps climbing.
	for i := range 22 {
		if _, err := reg.RestartByKey(jsregistry.HookKey(key), jsregistry.ReasonPanic); err != nil {
			t.Fatalf("restart panic #%d: %v", i, err)
		}
	}
	mi, _ = reg.Get(jsregistry.HookKey(key))
	if got := len(mi.History); got != 20 {
		t.Errorf("History length capped: got %d, want 20", got)
	}
	if got := mi.RestartsByReason[jsregistry.ReasonManual]; got != 3 {
		t.Errorf("RestartsByReason[manual] preserved: got %d, want 3", got)
	}
	if got := mi.RestartsByReason[jsregistry.ReasonPanic]; got != 22 {
		t.Errorf("RestartsByReason[panic]: got %d, want 22", got)
	}
	// Newest event sits at the tail (oldest-first storage); first three manual
	// events should have been evicted.
	if last := mi.LastRestart(); last.Reason != jsregistry.ReasonPanic {
		t.Errorf("LastRestart.Reason: got %q, want panic", last.Reason)
	}
	for i, ev := range mi.History {
		if ev.Reason != jsregistry.ReasonPanic {
			t.Errorf("History[%d]: got %q, want panic (manual entries should be evicted)", i, ev.Reason)
		}
	}
}

// js-registry.R4
func TestRegistry_RestartByKey_UnknownHook(t *testing.T) {
	reg := jsregistry.NewRegistry()
	if _, err := reg.RestartByKey(jsregistry.HookKey(types.NamespacedName{Name: "ghost"}), jsregistry.ReasonManual); err == nil {
		t.Fatal("expected error for unknown hook")
	}
}

func TestRegistry_Get(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "g"}
	t.Cleanup(func() { reg.Drop(jsregistry.HookKey(key)) })

	if _, ok := reg.Get(jsregistry.HookKey(key)); ok {
		t.Fatal("Get must return false for unknown key")
	}
	src := []byte(`function config(){return {}}`)
	mi, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{Source: src, SourceHash: "x"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := reg.Get(jsregistry.HookKey(key))
	if !ok || got != mi {
		t.Fatal("Get must return the live ManagedVM")
	}
}

func TestRegistry_Drop(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "h3"}

	src := []byte(`function config(){return {}}`)
	if _, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{Source: src, SourceHash: "x"}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len: got %d, want 1", reg.Len())
	}
	reg.Drop(jsregistry.HookKey(key))
	if reg.Len() != 0 {
		t.Fatalf("Len after drop: got %d, want 0", reg.Len())
	}
	reg.Drop(jsregistry.HookKey(key))
}

// js-registry.R4
// js-execution.R4
func TestRegistry_CancelledCall_IsCancelledAndRestartRebuildsDeadVM(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "k"}
	if _, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), jsregistry.BuildOptions{
		Source:     []byte("function spin() { while (true) {} }\nfunction ok() { return 1; }"),
		SourceHash: "h",
	}); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, _, err := reg.Call(ctx, jsregistry.HookKey(key), func(ctx context.Context, vm *jsengine.VM) error {
		_, err := vm.CallExport(ctx, "spin")
		return err
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Outcome != jsregistry.OutcomeCancelled || !errors.Is(res.Err, jsengine.ErrCancelled) {
		t.Fatalf("outcome %v err %v, want cancelled / ErrCancelled", res.Outcome, res.Err)
	}

	// The module is closed; restart closes the dead VM without a panic and
	// the rebuilt one runs.
	if _, err := reg.RestartByKey(jsregistry.HookKey(key), jsregistry.ReasonTimeout); err != nil {
		t.Fatalf("RestartByKey: %v", err)
	}
	res, _, _ = reg.Call(context.Background(), jsregistry.HookKey(key), func(ctx context.Context, vm *jsengine.VM) error {
		_, err := vm.CallExport(ctx, "ok")
		return err
	})
	if res.Outcome != jsregistry.OutcomeOK {
		t.Fatalf("rebuilt VM: outcome %v err %v", res.Outcome, res.Err)
	}

	// Dropping a dead VM must not panic either.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	_, _, _ = reg.Call(ctx2, jsregistry.HookKey(key), func(ctx context.Context, vm *jsengine.VM) error {
		_, err := vm.CallExport(ctx, "spin")
		return err
	})
	reg.Drop(jsregistry.HookKey(key))
}

// js-registry.R5
// js-registry.R12
func TestRegistry_GetOrLoad_RebuildsOnLimitsChange(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "lim"}
	t.Cleanup(func() { reg.Drop(jsregistry.HookKey(key)) })
	src := []byte(`function ok(){return 1}`)
	opts := jsregistry.BuildOptions{Source: src, SourceHash: "same", Limits: jsengine.Limits{MemoryMB: 16, TimeoutSeconds: 5}}

	mi, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), opts)
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}

	same := opts
	same.Limits = jsengine.Limits{MemoryMB: 16, TimeoutSeconds: 5}
	if mi2, restarted, _ := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), same); restarted || mi2 != mi {
		t.Fatalf("identical limits must not rebuild (restarted=%v)", restarted)
	}

	opts.Limits.MemoryMB = 64
	mi3, restarted, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), opts)
	if err != nil {
		t.Fatalf("GetOrLoad changed limits: %v", err)
	}
	if !restarted || mi3 == mi {
		t.Fatal("changed limits must rebuild the VM")
	}
	if got := mi3.VM.Limits().MemoryMB; got != 64 {
		t.Errorf("rebuilt VM MemoryMB = %d, want 64", got)
	}
	if got := mi3.LastRestart().Reason; got != jsregistry.ReasonLimitsChanged {
		t.Errorf("restart reason = %q, want %q", got, jsregistry.ReasonLimitsChanged)
	}
}

// js-registry.R13
func TestRegistry_RestartByKey_BuildHasDeadline(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := types.NamespacedName{Name: "dl"}
	t.Cleanup(func() { reg.Drop(jsregistry.HookKey(key)) })
	builds := 0
	opts := jsregistry.BuildOptions{
		Source:     []byte(`function ok(){return 1}`),
		SourceHash: "h",
		Limits:     jsengine.Limits{TimeoutSeconds: 1},
		// The first build passes; every later build hangs until its ctx ends.
		PostBuild: func(ctx context.Context, vm *jsengine.VM) (any, error) {
			builds++
			if builds == 1 {
				return nil, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	mi, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(key), opts)
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}

	done := make(chan error, 1)
	go func() { _, err := reg.RestartByKey(jsregistry.HookKey(key), jsregistry.ReasonManual); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RestartByKey: want error from the timed-out build")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RestartByKey did not return: rescue build has no deadline")
	}
	if cur, _ := reg.Get(jsregistry.HookKey(key)); cur != mi {
		t.Error("failed rescue build must keep the old VM installed")
	}
}

// TestRegistry_Concurrent_CallRestartDrop runs N goroutines on Registry.Call
// against concurrent RestartByKey, GetOrLoad rebuilds and Drop, under -race
// (make test).
//
// js-registry.R6
// js-registry.R7
// js-registry.R8
// js-execution.R7
func TestRegistry_Concurrent_CallRestartDrop(t *testing.T) {
	const (
		callers       = 8
		callsPerCall  = 15
		restarts      = 4
		limitRebuilds = 3
		dropCycles    = 3
	)
	reg := jsregistry.NewRegistry()
	keyA := types.NamespacedName{Name: "conc-a"}
	keyB := types.NamespacedName{Name: "conc-b"}
	t.Cleanup(func() { reg.Drop(jsregistry.HookKey(keyA)); reg.Drop(jsregistry.HookKey(keyB)) })

	var (
		builds, maxBuilds atomic.Int32 // concurrent PostBuild runs for keyA (R7)
		inCall            sync.Map     // *jsengine.VM -> *atomic.Int32 (R8, js-execution.R7)
		overlap           atomic.Int32 // two calls on one VM at once
	)
	optsA := func(mem int32) jsregistry.BuildOptions {
		return jsregistry.BuildOptions{
			Source:     []byte(`function ping(){ return 1 }`),
			SourceHash: "conc",
			Limits:     jsengine.Limits{MemoryMB: mem, TimeoutSeconds: 30},
			PostBuild: func(ctx context.Context, vm *jsengine.VM) (any, error) {
				n := builds.Add(1)
				for {
					m := maxBuilds.Load()
					if n <= m || maxBuilds.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				builds.Add(-1)
				return nil, nil
			},
		}
	}
	if _, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(keyA), optsA(16)); err != nil {
		t.Fatalf("GetOrLoad A: %v", err)
	}
	if _, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(keyB), jsregistry.BuildOptions{
		Source: []byte(`function ping(){ return 1 }`), SourceHash: "b",
	}); err != nil {
		t.Fatalf("GetOrLoad B: %v", err)
	}

	call := func(key types.NamespacedName) error {
		res, _, err := reg.Call(context.Background(), jsregistry.HookKey(key), func(ctx context.Context, vm *jsengine.VM) error {
			c, _ := inCall.LoadOrStore(vm, new(atomic.Int32))
			if c.(*atomic.Int32).Add(1) > 1 {
				overlap.Add(1)
			}
			defer c.(*atomic.Int32).Add(-1)
			_, err := vm.CallExport(ctx, "ping")
			return err
		})
		if err != nil {
			return err
		}
		if res.Outcome != jsregistry.OutcomeOK {
			return fmt.Errorf("outcome %v panic %v err %v", res.Outcome, res.Panic, res.Err)
		}
		return nil
	}

	// R6: a call blocked on key B's VM lock-free path must not stall key A's
	// registry operations; hold B inside a call while A is used and rebuilt.
	release := make(chan struct{})
	entered := make(chan struct{})
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		_, _, _ = reg.Call(context.Background(), jsregistry.HookKey(keyB), func(ctx context.Context, vm *jsengine.VM) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	// Phase 1: no Drop, so the build lock of keyA stays one mutex.
	errc := make(chan error, 1024)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			for range callsPerCall {
				if err := call(keyA); err != nil {
					errc <- fmt.Errorf("call: %w", err)
					return
				}
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			for range restarts {
				if _, err := reg.RestartByKey(jsregistry.HookKey(keyA), jsregistry.ReasonManual); err != nil {
					errc <- fmt.Errorf("restart: %w", err)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for i := range limitRebuilds {
			if _, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(keyA), optsA(int32(24+i))); err != nil {
				errc <- fmt.Errorf("getorload: %w", err)
				return
			}
		}
	})
	// Key B is busy in a call the whole time; key A's work above and a Get
	// on B must still complete.
	wg.Go(func() {
		for range 100 {
			reg.Get(jsregistry.HookKey(keyB))
			reg.Len()
		}
	})
	wg.Wait()
	close(release)
	<-bDone
	if got := maxBuilds.Load(); got > 1 {
		t.Errorf("R7: %d concurrent builds for one key, want 1", got)
	}

	// Phase 2: Drop and reload while calls run. A call may find the key
	// gone (ErrUnknownKey); anything else is a failure.
	var wg2 sync.WaitGroup
	for range callers {
		wg2.Go(func() {
			for range callsPerCall {
				if err := call(keyA); err != nil && !errors.Is(err, jsregistry.ErrUnknownKey) {
					errc <- fmt.Errorf("call after drop: %w", err)
					return
				}
			}
		})
	}
	wg2.Go(func() {
		for range dropCycles {
			reg.Drop(jsregistry.HookKey(keyA))
			if _, _, err := reg.GetOrLoad(context.Background(), jsregistry.HookKey(keyA), optsA(16)); err != nil {
				errc <- fmt.Errorf("reload: %w", err)
				return
			}
		}
	})
	wg2.Go(func() {
		for range restarts {
			if _, err := reg.RestartByKey(jsregistry.HookKey(keyA), jsregistry.ReasonManual); err != nil && !errors.Is(err, jsregistry.ErrUnknownKey) {
				errc <- fmt.Errorf("restart after drop: %w", err)
				return
			}
		}
	})
	wg2.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if n := overlap.Load(); n != 0 {
		t.Errorf("R8: %d overlapping calls on one VM", n)
	}
}

// js-registry.R14
func TestRegistry_SameNameInBothKindsCoexists(t *testing.T) {
	reg := jsregistry.NewRegistry()
	name := types.NamespacedName{Name: "foo"}
	hook, adm := jsregistry.HookKey(name), jsregistry.AdmissionKey(name)
	t.Cleanup(func() { reg.Drop(hook); reg.Drop(adm) })

	miH, _, err := reg.GetOrLoad(context.Background(), hook, jsregistry.BuildOptions{Source: []byte(`globalThis.who = "hook"`), SourceHash: "h"})
	if err != nil {
		t.Fatalf("GetOrLoad hook: %v", err)
	}
	miA, _, err := reg.GetOrLoad(context.Background(), adm, jsregistry.BuildOptions{Source: []byte(`globalThis.who = "admission"`), SourceHash: "a"})
	if err != nil {
		t.Fatalf("GetOrLoad admission: %v", err)
	}
	if miH == miA || reg.Len() != 2 {
		t.Fatalf("kinds share an entry: same=%v len=%d", miH == miA, reg.Len())
	}
	if got, _ := reg.Get(hook); got != miH {
		t.Fatal("JSHook VM was replaced by the JSAdmission of the same name")
	}
	reg.Drop(adm)
	if _, ok := reg.Get(hook); !ok {
		t.Fatal("dropping the JSAdmission dropped the JSHook")
	}
}
