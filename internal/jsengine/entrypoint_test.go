package jsengine

import (
	"context"
	"testing"
)

// The engine finds an entry point as a property of globalThis and nowhere
// else. That is a narrow contract, and a script written in modern style
// misses it, so it is written down and held here rather than left to a doc
// comment.
//
// js-execution.R18
func TestEntrypoint_IsAPropertyOfGlobalThis(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		found bool
	}{
		{"function declaration", `function handle(x) { return { n: x.n + 1 }; }`, true},
		{"var assignment", `var handle = function (x) { return { n: x.n + 1 }; };`, true},
		{"explicit globalThis", `globalThis.handle = (x) => ({ n: x.n + 1 });`, true},

		// const, let and class go into the global lexical environment, which
		// is not reachable through globalThis. The function exists and is
		// callable from inside the script, but the engine does not see it.
		{"const", `const handle = (x) => ({ n: x.n + 1 });`, false},
		{"let", `let handle = (x) => ({ n: x.n + 1 });`, false},

		// Anything a bundler wraps in a closure is local to that closure.
		{"wrapped in an IIFE", `(() => { function handle(x) { return { n: x.n + 1 }; } })();`, false},
		{"on a namespace object", `var bundle = { handle: (x) => ({ n: x.n + 1 }) };`, false},

		{"not a function", `var handle = 42;`, false},
		{"absent", `var other = 1;`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vm := newVM(t, Limits{})
			if err := vm.LoadModule(context.Background(), "e.js", tc.src); err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got := vm.HasExport("handle"); got != tc.found {
				t.Fatalf("HasExport(handle) = %v, want %v", got, tc.found)
			}

			var out struct{ N int }
			err := vm.Invoke(context.Background(), "handle", map[string]int{"n": 1}, &out)
			if tc.found {
				if err != nil || out.N != 2 {
					t.Fatalf("Invoke: out=%+v err=%v, want N=2", out, err)
				}
				return
			}
			if err == nil {
				t.Fatal("Invoke must fail for an entry point the engine does not see")
			}
		})
	}
}
