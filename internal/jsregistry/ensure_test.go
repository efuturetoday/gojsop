package jsregistry_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsregistry"
)

func hookKey(name string) jsregistry.Key {
	return jsregistry.HookKey(types.NamespacedName{Name: name})
}

func okOpts(hash string) jsregistry.BuildOptions {
	return jsregistry.BuildOptions{Source: []byte(`function ping(){ return 1 }`), SourceHash: hash}
}

// waitFor polls Ensure until the state kind is reached.
func waitFor(t *testing.T, reg *jsregistry.Registry, key jsregistry.Key, opts jsregistry.BuildOptions, want jsregistry.StateKind) jsregistry.State {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st := reg.Ensure(key, opts)
		if st.Kind == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %v, want %v", st.Kind, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// js-registry.R15
// js-registry.R18
func TestRegistry_Ensure_BuildsAsyncAndNotifies(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := hookKey("async")
	t.Cleanup(func() { reg.Drop(key) })
	ctx := t.Context()
	notes := reg.Watch(ctx, jsregistry.KindJSHook)

	opts := okOpts("h1")
	started := make(chan struct{})
	release := make(chan struct{})
	opts.PostBuild = func(context.Context, *jsengine.VM) (any, error) {
		close(started)
		<-release
		return nil, nil
	}
	if st := reg.Ensure(key, opts); st.Kind != jsregistry.StateBuilding {
		t.Fatalf("first Ensure: %v, want Building", st.Kind)
	}
	<-started
	// Single flight: a second Ensure with the same options joins the build.
	if st := reg.Ensure(key, opts); st.Kind != jsregistry.StateBuilding {
		t.Fatalf("second Ensure: %v, want Building", st.Kind)
	}
	close(release)
	select {
	case got := <-notes:
		if got != key {
			t.Fatalf("notified %v, want %v", got, key)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification after the build ended")
	}
	st := reg.Ensure(key, opts)
	if st.Kind != jsregistry.StateReady || st.VM == nil {
		t.Fatalf("after build: %v, want Ready with VM", st.Kind)
	}
}

// js-registry.R13
// js-registry.R15
func TestRegistry_Ensure_HangingBuildDoesNotBlockOtherKeys(t *testing.T) {
	reg := jsregistry.NewRegistry()
	hang, other := hookKey("hang"), hookKey("other")
	t.Cleanup(func() { reg.Drop(hang); reg.Drop(other) })

	start := time.Now()
	st := reg.Ensure(hang, jsregistry.BuildOptions{
		Source: []byte(`while(true){}`), SourceHash: "h", Limits: jsengine.Limits{TimeoutSeconds: 1},
	})
	if st.Kind != jsregistry.StateBuilding || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("Ensure of a hanging source: %v after %v, want Building at once", st.Kind, time.Since(start))
	}
	waitFor(t, reg, other, okOpts("o"), jsregistry.StateReady)
	// The build deadline from the limits ends the hang; the key is Broken.
	broken := waitFor(t, reg, hang, jsregistry.BuildOptions{
		Source: []byte(`while(true){}`), SourceHash: "h", Limits: jsengine.Limits{TimeoutSeconds: 1},
	}, jsregistry.StateBroken)
	if broken.Err == nil || broken.Attempts != 1 {
		t.Fatalf("Broken: err=%v attempts=%d, want an error and 1 attempt", broken.Err, broken.Attempts)
	}
}

// js-registry.R16
func TestRegistry_Ensure_NewOptsCancelRunningBuild(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := hookKey("cancel")
	t.Cleanup(func() { reg.Drop(key) })

	stuck := jsregistry.BuildOptions{Source: []byte(`while(true){}`), SourceHash: "stuck"} // default limit: 30 s
	if st := reg.Ensure(key, stuck); st.Kind != jsregistry.StateBuilding {
		t.Fatalf("Ensure stuck: %v", st.Kind)
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if st := reg.Ensure(key, okOpts("fixed")); st.Kind != jsregistry.StateBuilding {
		t.Fatalf("Ensure fixed: %v, want Building", st.Kind)
	}
	st := waitFor(t, reg, key, okOpts("fixed"), jsregistry.StateReady)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the new build waited %v for the stuck one: it was not cancelled", time.Since(start))
	}
	if st.VM.Opts.SourceHash != "fixed" {
		t.Fatalf("installed hash %q, want fixed", st.VM.Opts.SourceHash)
	}
}

// js-registry.R17
func TestRegistry_Ensure_BrokenBacksOffAndSourceChangeRebuildsAtOnce(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := hookKey("broken")
	t.Cleanup(func() { reg.Drop(key) })

	bad := jsregistry.BuildOptions{
		Source: []byte(`throw new Error("boom")`), SourceHash: "bad",
		Backoff: jsregistry.Backoff{Base: 100 * time.Millisecond, Max: time.Second},
	}
	first := waitFor(t, reg, key, bad, jsregistry.StateBroken)
	if first.Attempts != 1 || first.Err == nil {
		t.Fatalf("first failure: attempts=%d err=%v", first.Attempts, first.Err)
	}
	// Inside the backoff nothing is retried.
	if st := reg.Ensure(key, bad); st.Kind != jsregistry.StateBroken || st.Attempts != 1 {
		t.Fatalf("Ensure inside backoff: %v attempts=%d, want Broken with 1", st.Kind, st.Attempts)
	}
	// After the backoff the next Ensure retries; the next failure counts up.
	time.Sleep(time.Until(first.NextTry) + 10*time.Millisecond)
	reg.Ensure(key, bad)
	var second jsregistry.State
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		second = reg.Ensure(key, bad)
		if second.Kind == jsregistry.StateBroken && second.Attempts == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if second.Attempts != 2 {
		t.Fatalf("second failure: attempts=%d, want 2", second.Attempts)
	}
	if d := time.Until(second.NextTry); d <= 0 {
		t.Fatalf("second NextTry is not in the future (%v)", d)
	}
	// A changed source does not wait for the backoff.
	good := okOpts("good")
	good.Backoff = bad.Backoff
	if st := reg.Ensure(key, good); st.Kind != jsregistry.StateBuilding {
		t.Fatalf("Ensure with a new source: %v, want Building at once", st.Kind)
	}
	waitFor(t, reg, key, good, jsregistry.StateReady)
}

// js-registry.R17
func TestBackoff_DoublesCapsAndJitters(t *testing.T) {
	b := jsregistry.Backoff{Base: time.Second, Max: 10 * time.Second}
	for attempts, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 5: 10 * time.Second, 40: 10 * time.Second} {
		for range 50 {
			got := b.Delay(attempts)
			if got < want/2 || got > want {
				t.Fatalf("Delay(%d) = %v, want in [%v, %v]", attempts, got, want/2, want)
			}
		}
	}
	if d := (jsregistry.Backoff{}).Delay(1); d < jsregistry.DefaultBackoffBase/2 || d > jsregistry.DefaultBackoffBase {
		t.Fatalf("zero Backoff Delay(1) = %v, want about the default base", d)
	}
}

// js-registry.R18
func TestRegistry_Watch_DeliversOnlyOwnKind(t *testing.T) {
	reg := jsregistry.NewRegistry()
	name := types.NamespacedName{Name: "k"}
	hook, adm := jsregistry.HookKey(name), jsregistry.AdmissionKey(name)
	t.Cleanup(func() { reg.Drop(hook); reg.Drop(adm) })
	ctx := t.Context()
	hooks := reg.Watch(ctx, jsregistry.KindJSHook)
	adms := reg.Watch(ctx, jsregistry.KindJSAdmission)

	reg.Ensure(adm, okOpts("a"))
	select {
	case got := <-adms:
		if got != adm {
			t.Fatalf("admission channel got %v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no admission notification")
	}
	select {
	case got := <-hooks:
		t.Fatalf("hook channel got %v for an admission build", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRegistry_Concurrent_EnsureCallDrop hammers Ensure with changing options
// against Call and Drop of one key under -race (make test).
//
// js-registry.R7
// js-registry.R8
// js-registry.R16
func TestRegistry_Concurrent_EnsureCallDrop(t *testing.T) {
	reg := jsregistry.NewRegistry()
	key := hookKey("race")
	t.Cleanup(func() { reg.Drop(key) })
	opts := func(mem int32) jsregistry.BuildOptions {
		o := okOpts("race")
		o.Limits = jsengine.Limits{MemoryMB: mem, TimeoutSeconds: 30}
		return o
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range 4 {
		wg.Go(func() {
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				reg.Ensure(key, opts(int32(16+(n+i)%3)))
				time.Sleep(time.Millisecond)
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _, _ = reg.Call(context.Background(), key, func(ctx context.Context, vm *jsengine.VM) error {
					_, err := vm.CallExport(ctx, "ping")
					return err
				})
			}
		})
	}
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			time.Sleep(40 * time.Millisecond)
			reg.Drop(key)
		}
	})
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()
	reg.Drop(key)
	if reg.Len() != 0 {
		t.Fatalf("Len = %d after Drop", reg.Len())
	}
}
