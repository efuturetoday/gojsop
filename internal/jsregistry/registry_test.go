package jsregistry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsregistry/registrytest"
	"github.com/o-haase/gojsop/internal/jsrun"
)

// js-registry.R5
func TestRegistry_EveryCallStartsFromSnapshot(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "h1"})
	t.Cleanup(func() { reg.Drop(key) })

	src := []byte(`globalThis.counter = 0; function inc(){ return ++counter }`)
	opts := jsrun.Spec{Source: src, SourceHash: "abc123"}

	p1, restarted, err := registrytest.GetOrLoad(reg, context.Background(), key, opts)
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if !restarted {
		t.Fatal("expected restarted=true on first load")
	}
	if got := len(p1.Recoveries.Recent); got != 0 {
		t.Errorf("History length: got %d, want 0", got)
	}

	// Every call restores a fresh instance from the same snapshot, so a
	// top-level counter is back at its post-top-level-eval value every time,
	// whatever earlier calls did.
	var n int
	if res, err := reg.Invoke(context.Background(), key, "inc", nil, &n); err != nil || res.Outcome != jsrun.OutcomeOK || n != 1 {
		t.Fatalf("first call: res=%+v err=%v n=%d, want n=1", res, err, n)
	}
	if res, err := reg.Invoke(context.Background(), key, "inc", nil, &n); err != nil || res.Outcome != jsrun.OutcomeOK || n != 1 {
		t.Fatalf("second call: res=%+v err=%v n=%d, want n=1 again", res, err, n)
	}

	p2, restarted2, err := registrytest.GetOrLoad(reg, context.Background(), key, opts)
	if err != nil {
		t.Fatalf("GetOrLoad #2: %v", err)
	}
	if restarted2 {
		t.Fatal("expected restarted=false when the spec is unchanged")
	}
	if p2 != p1 {
		t.Fatal("expected the identical Prepared pointer when the spec is unchanged")
	}
}

// js-registry.R3
// js-sources.R3
func TestRegistry_RestartOnSourceChange(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "h2"})
	t.Cleanup(func() { reg.Drop(key) })

	src1 := []byte(`function tag(){ return "v1" }`)
	src2 := []byte(`function tag(){ return "v2" }`)

	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, jsrun.Spec{Source: src1, SourceHash: "h1"}); err != nil {
		t.Fatalf("load v1: %v", err)
	}
	var tag string
	if _, err := reg.Invoke(context.Background(), key, "tag", nil, &tag); err != nil || tag != "v1" {
		t.Fatalf("v1 tag: got %q, err %v", tag, err)
	}

	p2, restarted, err := registrytest.GetOrLoad(reg, context.Background(), key, jsrun.Spec{Source: src2, SourceHash: "h2"})
	if err != nil {
		t.Fatalf("load v2: %v", err)
	}
	if !restarted {
		t.Fatal("expected restarted=true on source change")
	}
	if got := len(p2.Recoveries.Recent); got != 1 {
		t.Errorf("History length: got %d, want 1", got)
	}
	if last := p2.Recoveries.Last(); last.Reason != jsrun.ReasonSourceChanged {
		t.Errorf("LastRestart.Reason: got %q", last.Reason)
	}
	if got := p2.Recoveries.ByReason[jsrun.ReasonSourceChanged]; got != 1 {
		t.Errorf("RestartsByReason[source-changed]: got %d, want 1", got)
	}
	var tag2 string
	if _, err := reg.Invoke(context.Background(), key, "tag", nil, &tag2); err != nil || tag2 != "v2" {
		t.Fatalf("v2 tag: got %q, err %v", tag2, err)
	}
}

// A ResetToken change rebuilds the script the same way a source change does,
// without the caller resupplying anything but the token: effective limits and
// the rest of the cached spec carry over. This replaces the old
// Restart-by-key rescue path, which no longer exists: a prepared script is
// only ever (re)built through Ensure.
//
// js-registry.R4
// js-registry.R9
func TestRegistry_Ensure_ResetTokenRebuildsFromCachedSpec(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "rescue"})
	t.Cleanup(func() { reg.Drop(key) })

	src := []byte(`function ok(){return 1}`)
	spec := jsrun.Spec{Source: src, SourceHash: "h1", Limits: jsengine.Limits{MemoryMB: 8}, ResetToken: "a"}
	p1, _, err := registrytest.GetOrLoad(reg, context.Background(), key, spec)
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}

	spec.ResetToken = "b"
	p2, restarted, err := registrytest.GetOrLoad(reg, context.Background(), key, spec)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !restarted || p2 == p1 {
		t.Fatal("a new reset token must install a fresh Prepared")
	}
	if last := p2.Recoveries.Last(); last.Reason != jsrun.ReasonManual {
		t.Errorf("LastRestart.Reason: got %q, want %q", last.Reason, jsrun.ReasonManual)
	}
	if got := len(p2.Recoveries.Recent); got != 1 {
		t.Errorf("History length: got %d, want 1", got)
	}
	if got := p2.Recoveries.ByReason[jsrun.ReasonManual]; got != 1 {
		t.Errorf("RestartsByReason[manual]: got %d, want 1", got)
	}
	if p2.Limits.MemoryMB != 8 {
		t.Errorf("limits not preserved: got MemoryMB=%d", p2.Limits.MemoryMB)
	}
}

// TestRegistry_RecoveryHistory_RingAndCounters proves the per-reason counter
// increments correctly across many recoveries and that History is capped at
// historyCap (20) with the oldest event evicted on overflow. Only Ensure
// drives a recovery now (source changed, limits changed, manual reset); a
// call outcome never does.
//
// status-conditions.R4
// js-registry.R9
// js-registry.R11
func TestRegistry_RecoveryHistory_RingAndCounters(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "ring"})
	t.Cleanup(func() { reg.Drop(key) })

	src := []byte(`function ok(){return 1}`)
	spec := jsrun.Spec{Source: src, SourceHash: "x"}
	waitFor(t, reg, key, spec, jsrun.PhaseReady)

	// Three manual resets (ResetToken changes) → counter == 3, history == 3.
	for i := range 3 {
		spec.ResetToken = fmt.Sprintf("manual-%d", i)
		waitFor(t, reg, key, spec, jsrun.PhaseReady)
	}
	p, _ := reg.Get(key)
	if got := p.Recoveries.ByReason[jsrun.ReasonManual]; got != 3 {
		t.Errorf("RestartsByReason[manual] after 3: got %d, want 3", got)
	}
	if got := len(p.Recoveries.Recent); got != 3 {
		t.Errorf("History length after 3: got %d, want 3", got)
	}

	// Push another 22 (total 25) via source changes — ring should cap at 20,
	// oldest evicted, counter keeps climbing.
	for i := range 22 {
		spec.SourceHash = fmt.Sprintf("src-%d", i)
		waitFor(t, reg, key, spec, jsrun.PhaseReady)
	}
	p, _ = reg.Get(key)
	if got := len(p.Recoveries.Recent); got != 20 {
		t.Errorf("History length capped: got %d, want 20", got)
	}
	if got := p.Recoveries.ByReason[jsrun.ReasonManual]; got != 3 {
		t.Errorf("RestartsByReason[manual] preserved: got %d, want 3", got)
	}
	if got := p.Recoveries.ByReason[jsrun.ReasonSourceChanged]; got != 22 {
		t.Errorf("RestartsByReason[source-changed]: got %d, want 22", got)
	}
	// Newest event sits at the tail (oldest-first storage); the three manual
	// events should have been evicted.
	if last := p.Recoveries.Last(); last.Reason != jsrun.ReasonSourceChanged {
		t.Errorf("LastRestart.Reason: got %q, want source-changed", last.Reason)
	}
	for i, ev := range p.Recoveries.Recent {
		if ev.Reason != jsrun.ReasonSourceChanged {
			t.Errorf("History[%d]: got %q, want source-changed (manual entries should be evicted)", i, ev.Reason)
		}
	}
}

func TestRegistry_Get(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "g"})
	t.Cleanup(func() { reg.Drop(key) })

	if _, ok := reg.Get(key); ok {
		t.Fatal("Get must return false for unknown key")
	}
	src := []byte(`function config(){return {}}`)
	p, _, err := registrytest.GetOrLoad(reg, context.Background(), key, jsrun.Spec{Source: src, SourceHash: "x"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := reg.Get(key)
	if !ok || got != p {
		t.Fatal("Get must return the live Prepared script")
	}
}

func TestRegistry_Drop(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "h3"})

	src := []byte(`function config(){return {}}`)
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, jsrun.Spec{Source: src, SourceHash: "x"}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("Len: got %d, want 1", reg.Len())
	}
	reg.Drop(key)
	if reg.Len() != 0 {
		t.Fatalf("Len after drop: got %d, want 0", reg.Len())
	}
	reg.Drop(key)
}

// A cancelled call just ends that call's instance: nothing is recovered, the
// prepared script is unchanged and the next call gets a fresh instance from
// it, whether or not the cancelled call's export ever returns.
//
// js-registry.R2
// js-execution.R3
// js-execution.R4
// js-execution.R13
func TestRegistry_CancelledCall_NextCallGetsFreshInstance(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "k"})
	t.Cleanup(func() { reg.Drop(key) })
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, jsrun.Spec{
		Source:     []byte("function spin() { while (true) {} }\nfunction ok() { return 1; }"),
		SourceHash: "h",
	}); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	before, _ := reg.Get(key)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := reg.Call(ctx, key, func(ctx context.Context, vm *jsengine.VM) error {
		_, err := vm.CallExport(ctx, "spin")
		return err
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Outcome != jsrun.OutcomeCancelled || !errors.Is(res.Err, jsengine.ErrCancelled) {
		t.Fatalf("outcome %v err %v, want cancelled / ErrCancelled", res.Outcome, res.Err)
	}

	// Nothing to recover: the next call still gets a fresh instance from the
	// very same prepared script.
	res, err = reg.Call(context.Background(), key, func(ctx context.Context, vm *jsengine.VM) error {
		_, err := vm.CallExport(ctx, "ok")
		return err
	})
	if res.Outcome != jsrun.OutcomeOK || err != nil {
		t.Fatalf("after the cancelled call: outcome %v err %v", res.Outcome, err)
	}
	after, _ := reg.Get(key)
	if after != before {
		t.Fatal("the prepared script must be unchanged after a cancelled call")
	}
	if got := len(after.Recoveries.Recent); got != 0 {
		t.Fatalf("Recoveries after a cancelled call: got %d entries, want 0", got)
	}
	if st, _ := reg.State(key); st.Phase != jsrun.PhaseReady {
		t.Fatalf("Phase after a cancelled call: %v, want Ready", st.Phase)
	}

	// Dropping a key right after a cancelled call must not panic.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	_, _ = reg.Call(ctx2, key, func(ctx context.Context, vm *jsengine.VM) error {
		_, err := vm.CallExport(ctx, "spin")
		return err
	})
	reg.Drop(key)
}

// js-registry.R5
// js-registry.R12
func TestRegistry_Ensure_RebuildsOnLimitsChange(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "lim"})
	t.Cleanup(func() { reg.Drop(key) })
	src := []byte(`function ok(){return 1}`)
	opts := jsrun.Spec{Source: src, SourceHash: "same", Limits: jsengine.Limits{MemoryMB: 16, TimeoutSeconds: 5}}

	p1, _, err := registrytest.GetOrLoad(reg, context.Background(), key, opts)
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}

	same := opts
	same.Limits = jsengine.Limits{MemoryMB: 16, TimeoutSeconds: 5}
	if p2, restarted, _ := registrytest.GetOrLoad(reg, context.Background(), key, same); restarted || p2 != p1 {
		t.Fatalf("identical limits must not rebuild (restarted=%v)", restarted)
	}

	opts.Limits.MemoryMB = 64
	p3, restarted, err := registrytest.GetOrLoad(reg, context.Background(), key, opts)
	if err != nil {
		t.Fatalf("GetOrLoad changed limits: %v", err)
	}
	if !restarted || p3 == p1 {
		t.Fatal("changed limits must rebuild the script")
	}
	if got := p3.Limits.MemoryMB; got != 64 {
		t.Errorf("rebuilt Limits.MemoryMB = %d, want 64", got)
	}
	if got := p3.Recoveries.Last().Reason; got != jsrun.ReasonLimitsChanged {
		t.Errorf("restart reason = %q, want %q", got, jsrun.ReasonLimitsChanged)
	}
}

// A rebuild is bounded by the timeout limit. Ensure returns at once, the key
// is Preparing while the old script keeps serving, and a build that runs into
// its deadline leaves the key Broken without a script (calls get
// ErrVMUnavailable).
//
// js-registry.R13
// js-registry.R19
func TestRegistry_Ensure_RebuildHasDeadlineAndFailureHoldsNoScript(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "dl"})
	t.Cleanup(func() { reg.Drop(key) })
	var builds atomic.Int32
	opts := jsrun.Spec{
		Source:     []byte(`function ok(){return 1}`),
		SourceHash: "h",
		Limits:     jsengine.Limits{TimeoutSeconds: 1},
		// The first build passes; every later build hangs until its ctx ends.
		PostBuild: func(ctx context.Context, vm jsrun.Script) (any, error) {
			if builds.Add(1) == 1 {
				return nil, nil
			}
			<-ctx.Done()
			return nil, ctx.Err()
		},
		ResetToken: "a",
	}
	waitFor(t, reg, key, opts, jsrun.PhaseReady)

	start := time.Now()
	opts.ResetToken = "b"
	if st := reg.Ensure(key, opts); st.Phase != jsrun.PhasePreparing {
		t.Fatalf("Ensure with a new token: %v, want Preparing", st.Phase)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Ensure waited %v for the build", time.Since(start))
	}
	if _, ok := reg.Get(key); !ok {
		t.Fatal("the old script must keep serving while the rebuild runs")
	}
	if _, err := reg.Invoke(context.Background(), key, "ok", nil, nil); err != nil {
		t.Fatalf("Invoke while Preparing: %v, want the old script to answer", err)
	}

	broken := waitFor(t, reg, key, opts, jsrun.PhaseFailed)
	if broken.Err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("Broken: err=%v after %v; the rebuild has no deadline", broken.Err, time.Since(start))
	}
	if _, ok := reg.Get(key); ok {
		t.Fatal("a broken key must hold no script")
	}
	if _, err := reg.Invoke(context.Background(), key, "ok", nil, nil); !errors.Is(err, jsrun.ErrVMUnavailable) {
		t.Fatalf("Invoke while Broken: %v, want ErrVMUnavailable", err)
	}
}

// TestRegistry_Concurrent_CallsBuildsAndDrop runs N goroutines on
// Registry.Call against concurrent Ensure rebuilds (manual reset, limits
// change) and Drop, under -race (make test). Calls of one key run in
// parallel (R1); only one build runs per key at a time (R7), and none of
// this ever races or panics.
//
// js-execution.R7
// js-registry.R6
// js-registry.R7
// js-registry.R8
func TestRegistry_Concurrent_CallsBuildsAndDrop(t *testing.T) {
	const (
		callers       = 8
		callsPerCall  = 15
		resets        = 4
		limitRebuilds = 3
		dropCycles    = 3
	)
	reg := jsregistry.NewRegistry()
	keyA := jsrun.HookKey(types.NamespacedName{Name: "conc-a"})
	keyB := jsrun.HookKey(types.NamespacedName{Name: "conc-b"})
	t.Cleanup(func() { reg.Drop(keyA); reg.Drop(keyB) })

	var builds, maxBuilds atomic.Int32 // concurrent PostBuild runs for keyA (R7)
	optsA := func(mem int32, reset string) jsrun.Spec {
		return jsrun.Spec{
			Source:     []byte(`function ping(){ return 1 }`),
			SourceHash: "conc",
			Limits:     jsengine.Limits{MemoryMB: mem, TimeoutSeconds: 30},
			ResetToken: reset,
			PostBuild: func(ctx context.Context, vm jsrun.Script) (any, error) {
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
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), keyA, optsA(16, "")); err != nil {
		t.Fatalf("GetOrLoad A: %v", err)
	}
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), keyB, jsrun.Spec{
		Source: []byte(`function ping(){ return 1 }`), SourceHash: "b",
	}); err != nil {
		t.Fatalf("GetOrLoad B: %v", err)
	}

	call := func(key jsrun.Key) error {
		res, err := reg.Call(context.Background(), key, func(ctx context.Context, vm *jsengine.VM) error {
			_, err := vm.CallExport(ctx, "ping")
			return err
		})
		if err = ignoreUnavailable(err); err != nil {
			return err
		}
		if res.Outcome != jsrun.OutcomeOK {
			return fmt.Errorf("outcome %v panic %v err %v", res.Outcome, res.Panic, res.Err)
		}
		return nil
	}

	// R6: a call blocked on key B must not stall key A's registry operations;
	// hold B inside a call while A is used and rebuilt.
	release := make(chan struct{})
	entered := make(chan struct{})
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		_, _ = reg.Call(context.Background(), keyB, func(ctx context.Context, vm *jsengine.VM) error {
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
	for i := range 2 {
		wg.Go(func() {
			for n := range resets {
				reg.Ensure(keyA, optsA(16, fmt.Sprintf("reset-%d-%d", i, n)))
			}
		})
	}
	wg.Go(func() {
		for i := range limitRebuilds {
			if _, _, err := registrytest.GetOrLoad(reg, context.Background(), keyA, optsA(int32(24+i), "")); err != nil {
				errc <- fmt.Errorf("getorload: %w", err)
				return
			}
		}
	})
	// Key B is busy in a call the whole time; key A's work above and a Get
	// on B must still complete.
	wg.Go(func() {
		for range 100 {
			reg.Get(keyB)
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
				if err := call(keyA); err != nil && !errors.Is(err, jsrun.ErrUnknownKey) {
					errc <- fmt.Errorf("call after drop: %w", err)
					return
				}
			}
		})
	}
	wg2.Go(func() {
		for range dropCycles {
			reg.Drop(keyA)
			if _, _, err := registrytest.GetOrLoad(reg, context.Background(), keyA, optsA(16, "")); err != nil {
				errc <- fmt.Errorf("reload: %w", err)
				return
			}
		}
	})
	wg2.Go(func() {
		for i := range resets {
			reg.Ensure(keyA, optsA(16, fmt.Sprintf("reset2-%d", i)))
		}
	})
	wg2.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
}

// js-registry.R14
// js-execution.R8
func TestRegistry_SameNameInBothKindsCoexists(t *testing.T) {
	reg := jsregistry.NewRegistry()
	name := types.NamespacedName{Name: "foo"}
	hook, adm := jsrun.HookKey(name), jsrun.AdmissionKey(name)
	t.Cleanup(func() { reg.Drop(hook); reg.Drop(adm) })

	pH, _, err := registrytest.GetOrLoad(reg, context.Background(), hook, jsrun.Spec{Source: []byte(`globalThis.who = "hook"`), SourceHash: "h"})
	if err != nil {
		t.Fatalf("GetOrLoad hook: %v", err)
	}
	pA, _, err := registrytest.GetOrLoad(reg, context.Background(), adm, jsrun.Spec{Source: []byte(`globalThis.who = "admission"`), SourceHash: "a"})
	if err != nil {
		t.Fatalf("GetOrLoad admission: %v", err)
	}
	if pH == pA || reg.Len() != 2 {
		t.Fatalf("kinds share an entry: same=%v len=%d", pH == pA, reg.Len())
	}
	if got, _ := reg.Get(hook); got != pH {
		t.Fatal("JSHook script was replaced by the JSAdmission of the same name")
	}
	reg.Drop(adm)
	if _, ok := reg.Get(hook); !ok {
		t.Fatal("dropping the JSAdmission dropped the JSHook")
	}
}

// ignoreUnavailable drops ErrVMUnavailable: a rebuild is in flight and the
// call is refused at once, which is the contract.
func ignoreUnavailable(err error) error {
	if errors.Is(err, jsrun.ErrVMUnavailable) {
		return nil
	}
	return err
}

// js-registry.R19
func TestRegistry_Call_WithoutScript_IsErrVMUnavailable(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "novm"})
	t.Cleanup(func() { reg.Drop(key) })
	noop := func(context.Context, *jsengine.VM) error { return nil }

	if _, err := reg.Call(context.Background(), key, noop); !errors.Is(err, jsrun.ErrUnknownKey) {
		t.Fatalf("never registered: %v, want ErrUnknownKey", err)
	}
	// Preparing: the first build runs and has no script yet.
	reg.Ensure(key, jsrun.Spec{Source: []byte(`while(true){}`), SourceHash: "h", Limits: jsengine.Limits{TimeoutSeconds: 1}})
	start := time.Now()
	if _, err := reg.Call(context.Background(), key, noop); !errors.Is(err, jsrun.ErrVMUnavailable) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Preparing: %v after %v, want ErrVMUnavailable at once", err, time.Since(start))
	}
	// Broken: the build ran into its deadline.
	waitFor(t, reg, key, jsrun.Spec{Source: []byte(`while(true){}`), SourceHash: "h", Limits: jsengine.Limits{TimeoutSeconds: 1}}, jsrun.PhaseFailed)
	if _, ok := reg.Get(key); ok {
		t.Fatal("a broken key must hold no script")
	}
	if _, err := reg.Call(context.Background(), key, noop); !errors.Is(err, jsrun.ErrVMUnavailable) {
		t.Fatalf("Broken: %v, want ErrVMUnavailable", err)
	}
	// A cancelled call leaves the key Ready: the instance it ran on is
	// thrown away, but the next call still gets a fresh one from the same
	// prepared script.
	ok := jsrun.Spec{Source: []byte("function spin() { while (true) {} }"), SourceHash: "ok"}
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, ok); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, _ := reg.Call(ctx, key, func(ctx context.Context, vm *jsengine.VM) error {
		_, err := vm.CallExport(ctx, "spin")
		return err
	})
	if res.Outcome != jsrun.OutcomeCancelled {
		t.Fatalf("outcome %v, want cancelled", res.Outcome)
	}
	if _, ok := reg.Get(key); !ok {
		t.Fatal("a cancelled call must not drop the prepared script")
	}
	if _, err := reg.Call(context.Background(), key, noop); err != nil {
		t.Fatalf("after the cancelled call: %v, want a fresh instance to serve the next call", err)
	}
}

// js-execution.R2
// js-execution.R4
// js-registry.R1
func TestRegistry_Invoke_ClassifiesOutcomes(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "inv"})
	t.Cleanup(func() { reg.Drop(key) })

	if _, err := reg.Invoke(context.Background(), key, "f", nil, nil); !errors.Is(err, jsrun.ErrUnknownKey) {
		t.Fatalf("unknown key: %v, want ErrUnknownKey", err)
	}
	src := []byte(`function f(x) { return {v: x}; } function boom() { throw new Error("boom"); }`)
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, jsrun.Spec{Source: src, SourceHash: "h"}); err != nil {
		t.Fatalf("load: %v", err)
	}

	var out struct{ V string }
	res, err := reg.Invoke(context.Background(), key, "f", "hi", &out)
	if err != nil || res.Outcome != jsrun.OutcomeOK || out.V != "hi" {
		t.Fatalf("ok call: res=%+v err=%v out=%+v", res, err, out)
	}
	res, err = reg.Invoke(context.Background(), key, "boom", nil, nil)
	if err != nil || res.Outcome != jsrun.OutcomeError || res.Err == nil {
		t.Fatalf("throwing call: res=%+v err=%v, want OutcomeError", res, err)
	}
}

// Every outcome of Invoke — a thrown error, a timeout, hitting the memory
// limit, or a wasm trap (a panicking host function) — just ends that call's
// instance. Nothing is recovered and the key's prepared script never
// changes: whatever the previous outcome was, the next call restores a
// fresh instance from the unchanged snapshot, so a top-level counter starts
// at its post-top-level-eval value every time.
//
// js-registry.R2
// js-registry.R19
// js-execution.R3
// js-execution.R4
func TestRegistry_Invoke_EveryOutcomeGetsAFreshInstance(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "selfrescue"})
	t.Cleanup(func() { reg.Drop(key) })
	src := []byte(`globalThis.n = 0;
function spin() { while (true) {} }
function ok() { return ++n }
function boom() { throw new Error("x") }
function hog() { var a = []; for (;;) a.push(new Array(10000).fill(1)) }
function crash() { crashNow() }`)
	host := jsengine.HostBinderFunc(func(h *jsengine.Host) error {
		h.Func("crashNow", func(context.Context, json.RawMessage) (any, error) { panic("host bug") })
		return nil
	})
	spec := jsrun.Spec{Source: src, SourceHash: "h", Limits: jsrun.Limits{MemoryMB: 4}, Host: host}
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, spec); err != nil {
		t.Fatalf("load: %v", err)
	}
	first, _ := reg.Get(key)

	assertUnchanged := func(t *testing.T, label string) {
		t.Helper()
		now, _ := reg.Get(key)
		if now != first {
			t.Fatalf("%s: the prepared script was replaced", label)
		}
		st, _ := reg.State(key)
		if st.Phase != jsrun.PhaseReady {
			t.Fatalf("%s: phase = %v, want Ready", label, st.Phase)
		}
		if got := len(st.Recoveries.Recent); got != 0 {
			t.Fatalf("%s: Recoveries = %d entries, want 0", label, got)
		}
	}

	res, err := reg.Invoke(context.Background(), key, "boom", nil, nil)
	if err != nil || res.Outcome != jsrun.OutcomeError {
		t.Fatalf("throwing call: res=%+v err=%v, want OutcomeError", res, err)
	}
	assertUnchanged(t, "after error")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err = reg.Invoke(ctx, key, "spin", nil, nil)
	if err != nil || res.Outcome != jsrun.OutcomeCancelled || !errors.Is(res.Err, jsengine.ErrCancelled) {
		t.Fatalf("spin: res=%+v err=%v, want OutcomeCancelled", res, err)
	}
	assertUnchanged(t, "after timeout")

	res, err = reg.Invoke(context.Background(), key, "hog", nil, nil)
	if err != nil || res.Outcome != jsrun.OutcomeMemoryLimit || !errors.Is(res.Err, jsengine.ErrOOM) {
		t.Fatalf("hog: res=%+v err=%v, want OutcomeMemoryLimit", res, err)
	}
	assertUnchanged(t, "after memory limit")

	res, err = reg.Invoke(context.Background(), key, "crash", nil, nil)
	if err != nil || res.Outcome != jsrun.OutcomePanic {
		t.Fatalf("crash: res=%+v err=%v, want OutcomePanic", res, err)
	}
	assertUnchanged(t, "after panic")

	// Whatever the previous outcome, the next call restores a fresh instance
	// from the unchanged snapshot: the top-level counter starts at 1 again.
	var n int
	if res, err = reg.Invoke(context.Background(), key, "ok", nil, &n); err != nil || res.Outcome != jsrun.OutcomeOK || n != 1 {
		t.Fatalf("fresh instance after every outcome: res=%+v err=%v n=%d, want n=1", res, err, n)
	}
}

// Another reset token prepares the script again with reason manual, the same
// path as a changed source; the same token does not.
//
// js-registry.R4
// js-registry.R5
func TestRegistry_Ensure_ResetTokenRebuildsAsManual(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "reset"})
	t.Cleanup(func() { reg.Drop(key) })
	spec := jsrun.Spec{Source: []byte(`function ok() { return 1 }`), SourceHash: "h", ResetToken: "a"}
	waitFor(t, reg, key, spec, jsrun.PhaseReady)
	first, _ := reg.Get(key)

	if st := reg.Ensure(key, spec); st.Phase != jsrun.PhaseReady {
		t.Fatalf("same token: %v, want Ready", st.Phase)
	}
	spec.ResetToken = "b"
	if st := reg.Ensure(key, spec); st.Phase != jsrun.PhasePreparing {
		t.Fatalf("new token: %v, want Preparing", st.Phase)
	}
	st := waitFor(t, reg, key, spec, jsrun.PhaseReady)
	if got := st.Recoveries.Last().Reason; got != jsrun.ReasonManual {
		t.Fatalf("last recovery = %q, want manual", got)
	}
	if again, _ := reg.Get(key); again == first {
		t.Fatal("a new token must install a fresh script")
	}
}

// js-registry.R1
func TestRegistry_CallsOfOneKeyRunInParallel(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := jsrun.HookKey(types.NamespacedName{Name: "parallel"})
	t.Cleanup(func() { reg.Drop(key) })

	entered := make(chan struct{})
	release := make(chan struct{})
	host := jsengine.HostBinderFunc(func(h *jsengine.Host) error {
		h.Func("block", func(context.Context, json.RawMessage) (any, error) {
			close(entered)
			<-release
			return nil, nil
		})
		return nil
	})
	spec := jsrun.Spec{
		Source:     []byte(`function blocked(){ block(); return 1 } function fast(){ return 2 }`),
		SourceHash: "h",
		Host:       host,
	}
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, spec); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}

	done := make(chan jsrun.Result, 1)
	go func() {
		res, err := reg.Invoke(context.Background(), key, "blocked", nil, nil)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first call never entered the host function")
	}

	// A second call of the same key completes while the first still blocks
	// inside its host function.
	var n int
	res, err := reg.Invoke(context.Background(), key, "fast", nil, &n)
	if err != nil || res.Outcome != jsrun.OutcomeOK || n != 2 {
		t.Fatalf("second call: res=%+v err=%v n=%d, want OutcomeOK/2", res, err, n)
	}

	close(release)
	select {
	case res := <-done:
		if res.Outcome != jsrun.OutcomeOK {
			t.Fatalf("first call: outcome %v, want OK", res.Outcome)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first call never finished")
	}
}

// js-registry.R20
func TestRegistry_Semaphore_BoundsConcurrentCalls(t *testing.T) {
	reg := jsregistry.New(jsregistry.Options{MaxConcurrentCalls: 1})
	key := jsrun.HookKey(types.NamespacedName{Name: "sem"})
	t.Cleanup(func() { reg.Drop(key) })

	entered := make(chan struct{})
	release := make(chan struct{})
	host := jsengine.HostBinderFunc(func(h *jsengine.Host) error {
		h.Func("block", func(context.Context, json.RawMessage) (any, error) {
			close(entered)
			<-release
			return nil, nil
		})
		return nil
	})
	spec := jsrun.Spec{
		Source:     []byte(`function blocked(){ block(); return 1 } function fast(){ return 2 }`),
		SourceHash: "h",
		Host:       host,
	}
	if _, _, err := registrytest.GetOrLoad(reg, context.Background(), key, spec); err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = reg.Invoke(context.Background(), key, "blocked", nil, nil)
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first call never entered the host function")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, err := reg.Invoke(ctx, key, "fast", nil, nil)
	if err != nil || res.Outcome != jsrun.OutcomeCancelled {
		t.Fatalf("second call while the one slot is taken: res=%+v err=%v, want OutcomeCancelled", res, err)
	}

	close(release)
	<-done

	var n int
	res, err = reg.Invoke(context.Background(), key, "fast", nil, &n)
	if err != nil || res.Outcome != jsrun.OutcomeOK || n != 2 {
		t.Fatalf("after the slot frees up: res=%+v err=%v n=%d", res, err, n)
	}
}
