package jsengine

import (
	"context"
	"testing"
	"time"
)

const benchSrc = `
function validate(req) {
  var bad = [];
  for (var c of req.object.spec.containers) {
    if (/:latest$/.test(c.image) || c.image.indexOf(":") < 0) bad.push(c.name);
  }
  return { allowed: bad.length === 0, bad: bad };
}
function spin() { for (;;) {} }
`

var benchReq = map[string]any{"object": map[string]any{"spec": map[string]any{"containers": []any{
	map[string]any{"name": "a", "image": "nginx:1.27"},
	map[string]any{"name": "b", "image": "redis:latest"},
}}}}

// BenchmarkVM_WarmCall is the cost of one call on a long-lived VM. It guards
// EXEC-3: the engine replaced CloseOnContextDone (4x on a warm call) with the
// interrupt handler, so a warm call must stay in the tens of microseconds.
// js-execution.R13
func BenchmarkVM_WarmCall(b *testing.B) {
	vm, err := New(Limits{})
	if err != nil {
		b.Fatal(err)
	}
	defer vm.Close()
	if err := vm.LoadModule(context.Background(), "p.js", benchSrc); err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	var out map[string]any
	b.ReportAllocs()
	for b.Loop() {
		if err := vm.Invoke(ctx, "validate", benchReq, &out); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVM_Timeout is a call that runs into a 5 ms deadline and the next call
// on the same VM. It guards that a timeout costs the deadline plus a little
// and leaves the VM usable, with no rebuild.
// js-execution.R13
func BenchmarkVM_Timeout(b *testing.B) {
	vm, err := New(Limits{})
	if err != nil {
		b.Fatal(err)
	}
	defer vm.Close()
	if err := vm.LoadModule(context.Background(), "p.js", benchSrc); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		err := vm.Invoke(ctx, "spin", nil, nil)
		cancel()
		if err == nil {
			b.Fatal("spin returned")
		}
		if err := vm.Invoke(context.Background(), "validate", benchReq, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVM_New is the cost of a fresh VM with the compiled module cached:
// what a rebuild after a trap, or a changed source, pays.
func BenchmarkVM_New(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		vm, err := New(Limits{})
		if err != nil {
			b.Fatal(err)
		}
		vm.Close()
	}
}
