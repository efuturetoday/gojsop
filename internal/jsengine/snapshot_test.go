package jsengine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func snapshotOf(t *testing.T, src string) *Snapshot {
	t.Helper()
	vm := newVM(t, Limits{})
	if err := vm.LoadModule(context.Background(), "snap.js", src); err != nil {
		t.Fatal(err)
	}
	s, err := vm.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func restore(t *testing.T, s *Snapshot) *VM {
	t.Helper()
	vm, err := s.NewVM(context.Background())
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	t.Cleanup(vm.Close)
	return vm
}

// js-execution.R16
// Every VM from one snapshot starts from the state after the top-level eval;
// what one call changes does not reach the next VM.
func TestSnapshot_EveryVMStartsFromTheSameState(t *testing.T) {
	s := snapshotOf(t, `var n = 0; function inc() { n++; return n; }`)
	for range 3 {
		vm := restore(t, s)
		for want := 1; want <= 2; want++ {
			got, err := vm.CallExport(context.Background(), "inc")
			if err != nil || got != jsonInt(want) {
				t.Fatalf("inc() = %q, %v; want %d", got, err, want)
			}
		}
	}
}

func jsonInt(n int) string { b, _ := json.Marshal(n); return string(b) }

// js-execution.R16
// A snapshot keeps the memory limit and the host functions of its VM.
func TestSnapshot_KeepsLimitsAndHost(t *testing.T) {
	vm := newVM(t, Limits{MemoryMB: 4})
	if err := vm.BindHost(HostBinderFunc(func(h *Host) error {
		h.Func("kube.echo", func(_ context.Context, arg json.RawMessage) (any, error) { return arg, nil })
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := vm.LoadModule(context.Background(), "h.js", `function f(x) { return kube.echo(x); }
		function big() { return "x".repeat(16 * 1024 * 1024).length; }`); err != nil {
		t.Fatal(err)
	}
	s, err := vm.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := restore(t, s)
	if got, err := r.CallExport(context.Background(), "f", map[string]int{"a": 1}); err != nil || got != `{"a":1}` {
		t.Fatalf("f() = %q, %v", got, err)
	}
	if _, err := r.CallExport(context.Background(), "big"); !errors.Is(err, ErrOOM) {
		t.Fatalf("big() err = %v, want ErrOOM", err)
	}
}

// js-execution.R16
// Restored VMs do not share the Math.random state of the snapshot.
func TestSnapshot_ReseedsMathRandom(t *testing.T) {
	s := snapshotOf(t, `function r() { return Math.random(); }`)
	a, err := restore(t, s).CallExport(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	b, err := restore(t, s).CallExport(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two restored VMs returned the same Math.random() %s", a)
	}
	var f float64
	if err := json.Unmarshal([]byte(a), &f); err != nil || f < 0 || f >= 1 {
		t.Fatalf("Math.random() = %s, want [0,1)", a)
	}
}

// js-execution.R16
// js-execution.R7
// js-registry.R8
// Restores of one snapshot run in parallel without touching each other.
func TestSnapshot_ConcurrentRestores(t *testing.T) {
	s := snapshotOf(t, `var seen = []; function add(x) { seen.push(x); return seen.length; }`)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			vm, err := s.NewVM(context.Background())
			if err != nil {
				errs <- err
				return
			}
			defer vm.Close()
			got, err := vm.CallExport(context.Background(), "add", i)
			if err != nil || got != "1" {
				errs <- errors.New("add() = " + got)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// js-execution.R16
// A snapshot of a small script keeps far less than the 1 MiB stack plus heap.
func TestSnapshot_IsSparse(t *testing.T) {
	s := snapshotOf(t, `function f() { return 1; }`)
	if s.Bytes() >= 1<<20 {
		t.Fatalf("snapshot holds %d bytes, want < 1 MiB", s.Bytes())
	}
}
