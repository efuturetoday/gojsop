package jslog_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jslog"
)

type recorder struct {
	mu    sync.Mutex
	lines []jslog.Line
}

func (r *recorder) Log(l jslog.Line) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, l)
}

func (r *recorder) all() []jslog.Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]jslog.Line(nil), r.lines...)
}

// run evaluates src in a VM with the console bound and the sink installed,
// and returns what the script wrote.
func run(t *testing.T, src string) []jslog.Line {
	t.Helper()
	vm, err := jsengine.New(jsengine.Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(vm.Close)

	if err := vm.BindHost(jslog.Binder{}); err != nil {
		t.Fatalf("BindHost: %v", err)
	}
	rec := &recorder{}
	ctx := jslog.WithSink(context.Background(), rec)
	if err := vm.LoadModule(ctx, "<test>", src); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	if _, err := vm.Eval(ctx, "<call>", "run()"); err != nil {
		t.Fatalf("run(): %v", err)
	}
	return rec.all()
}

// A script reaches its caller through console. Every level arrives, and the
// level decides how visible the line is.
//
// js-execution.R17
func TestConsole_EveryLevelReachesTheSink(t *testing.T) {
	got := run(t, `
		function run() {
			console.log("plain");
			console.info("info");
			console.debug("debug");
			console.warn("warn");
			console.error("error");
		}`)

	want := []jslog.Line{
		{Level: jslog.LevelInfo, Text: "plain"},
		{Level: jslog.LevelInfo, Text: "info"},
		{Level: jslog.LevelDebug, Text: "debug"},
		{Level: jslog.LevelWarn, Text: "warn"},
		{Level: jslog.LevelError, Text: "error"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, l := range got {
		if l.Level.Visible() != (l.Level == jslog.LevelWarn || l.Level == jslog.LevelError) {
			t.Errorf("level %q: Visible() is wrong", l.Level)
		}
	}
}

// console takes any number of arguments of any type, the way a JS author
// expects. The host function underneath takes exactly one.
//
// js-execution.R17
func TestConsole_JoinsEveryArgument(t *testing.T) {
	got := run(t, `
		function run() {
			console.log("obj:", {a: 1, b: [2, 3]}, "n:", 42, true, null);
		}`)
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1: %+v", len(got), got)
	}
	want := `obj: {"a":1,"b":[2,3]} n: 42 true null`
	if got[0].Text != want {
		t.Errorf("text:\n got %q\nwant %q", got[0].Text, want)
	}
}

// An Error logs its stack, which is the whole point when a hook fails.
//
// js-execution.R17
func TestConsole_LogsAnErrorReadably(t *testing.T) {
	got := run(t, `
		function run() {
			try { null.boom(); } catch (e) { console.error("failed:", e); }
		}`)
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].Text, "failed: ") {
		t.Errorf("prefix lost: %q", got[0].Text)
	}
	if !strings.Contains(got[0].Text, "TypeError") {
		t.Errorf("error text does not name the failure: %q", got[0].Text)
	}
}

// A line a script writes can be arbitrarily long; it ends up in an Event, so
// it is capped.
//
// js-execution.R17
func TestConsole_CapsOneLine(t *testing.T) {
	got := run(t, `function run() { console.log("x".repeat(100000)); }`)
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	if len(got[0].Text) > jslog.MaxTextBytes+len("… (truncated)") {
		t.Errorf("line not capped: %d bytes", len(got[0].Text))
	}
	if !strings.HasSuffix(got[0].Text, "(truncated)") {
		t.Error("a capped line must say so")
	}
}

// A call without a sink must not break the script: the caller forgot to
// install one, the script did nothing wrong.
//
// js-execution.R17
func TestConsole_WithoutSink_DoesNotFailTheScript(t *testing.T) {
	vm, err := jsengine.New(jsengine.Limits{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer vm.Close()

	if err := vm.BindHost(jslog.Binder{}); err != nil {
		t.Fatalf("BindHost: %v", err)
	}
	ctx := context.Background()
	if err := vm.LoadModule(ctx, "<test>", `function run() { console.log("nobody listens"); return 7; }`); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	out, err := vm.Eval(ctx, "<call>", "run()")
	if err != nil {
		t.Fatalf("run() failed without a sink: %v", err)
	}
	if strings.TrimSpace(out) != "7" {
		t.Errorf("result: got %q, want 7", out)
	}
}

// The raw host function is not reachable from user code: the shim deletes it
// after wrapping it, so a script cannot bypass the level and the cap.
//
// js-execution.R17
func TestConsole_RawHostFunctionIsHidden(t *testing.T) {
	got := run(t, `
		function run() {
			console.log("raw is " + typeof globalThis["__gojsop_console"]);
		}`)
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	if got[0].Text != "raw is undefined" {
		t.Errorf("raw host function still reachable: %q", got[0].Text)
	}
}
