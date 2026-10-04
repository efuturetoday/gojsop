// Package jslog gives a running script a voice. It binds a console object
// into every VM and routes what the script writes to a Sink that the caller
// put into the context of the call.
//
// Without it a script cannot say anything at all: the only other globals are
// kube.*, so a hook author has no way to trace their own code and no way to
// explain a failure to the person who deployed the hook (EXEC-10).
package jslog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/efuturetoday/gojsop/internal/jsengine"
)

// Level classifies one console line. It decides how visible the line is:
// info and debug reach the operator log, warn and error also reach the user
// through an Event on the hook or policy that wrote them.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Visible reports whether a line of this level is meant for the user of the
// CR and not only for the operator log.
func (l Level) Visible() bool { return l == LevelWarn || l == LevelError }

// MaxTextBytes caps one line. A script cannot be trusted to keep its output
// small, and a line can end up in an Event, which etcd stores.
const MaxTextBytes = 2048

// Line is one console call of a script, with its arguments already joined
// into Text by the JS side.
type Line struct {
	Level Level  `json:"level"`
	Text  string `json:"text"`
}

// Sink receives the console lines of one call. Implementations must be safe
// for concurrent use: calls of one script run in parallel, and each gets the
// sink its caller installed.
type Sink interface {
	Log(Line)
}

// SinkFunc adapts a plain function to Sink.
type SinkFunc func(Line)

// Log implements Sink.
func (f SinkFunc) Log(l Line) { f(l) }

// MaxVisibleEvents caps how many lines of one call a caller should turn into
// Events. A script in a hot event loop can write on every call, and every
// Event is an etcd write; the operator log keeps the rest.
const MaxVisibleEvents = 3

// Collector buffers the console lines of one call so the caller can route
// them once the call is over. Safe for concurrent use.
type Collector struct {
	mu    sync.Mutex
	lines []Line
}

// Log implements Sink.
func (c *Collector) Log(l Line) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, l)
}

// Lines returns what the script wrote, in order.
func (c *Collector) Lines() []Line {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Line(nil), c.lines...)
}

// Route hands every line to log and the first MaxVisibleEvents warn/error
// lines to event. Either callback may be nil.
func (c *Collector) Route(logLine func(Line), event func(Line)) {
	visible := 0
	for _, l := range c.Lines() {
		if logLine != nil {
			logLine(l)
		}
		if !l.Level.Visible() || event == nil {
			continue
		}
		if visible >= MaxVisibleEvents {
			continue
		}
		visible++
		event(l)
	}
}

type sinkKey struct{}

// WithSink returns a context whose script console writes to s. Callers
// install it on the context they hand to Runner.Invoke.
func WithSink(ctx context.Context, s Sink) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, sinkKey{}, s)
}

// FromContext returns the sink of this call, or nil when none was installed.
func FromContext(ctx context.Context) Sink {
	s, _ := ctx.Value(sinkKey{}).(Sink)
	return s
}

// hostFn is the single host function the console shim calls.
const hostFn = "__gojsop_console"

// Binder binds globalThis.console into a VM. It is a jsengine.HostBinder and
// composes with the kube binder through jsengine.Binders.
//
// The lines go to the Sink in the context of the running call, so one
// prepared script can serve calls of different callers.
type Binder struct{}

// Bind implements jsengine.HostBinder.
func (Binder) Bind(h *jsengine.Host) error {
	h.Func(hostFn, emit)
	h.Shim(consoleShim)
	return nil
}

// emit hands one line to the sink of the running call. A call with no sink
// is dropped rather than failed: a missing sink is the caller's omission,
// and a script must not break because of it.
func emit(ctx context.Context, arg json.RawMessage) (any, error) {
	var l Line
	if err := json.Unmarshal(arg, &l); err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	switch l.Level {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
	default:
		l.Level = LevelInfo
	}
	if len(l.Text) > MaxTextBytes {
		l.Text = l.Text[:MaxTextBytes] + "… (truncated)"
	}
	if s := FromContext(ctx); s != nil {
		s.Log(l)
	}
	return nil, nil
}

// consoleShim defines a variadic console over the one-argument host function.
// jsengine.Host.Func can only define a function of exactly one argument, but
// every JS author expects console.log("tried", obj) to work.
//
// Strings pass through; everything else becomes JSON, falling back to String
// for values JSON cannot hold (cycles, BigInt, undefined).
const consoleShim = `
globalThis.console = (function (emit) {
  function one(a) {
    if (typeof a === "string") return a;
    if (a instanceof Error) {
      var head = (a.name || "Error") + ": " + a.message;
      return a.stack ? head + "\n" + a.stack : head;
    }
    try {
      var s = JSON.stringify(a);
      return s === undefined ? String(a) : s;
    } catch (e) {
      return String(a);
    }
  }
  function at(level) {
    return function () {
      var parts = [];
      for (var i = 0; i < arguments.length; i++) parts.push(one(arguments[i]));
      emit({ level: level, text: parts.join(" ") });
    };
  }
  return { log: at("info"), info: at("info"), debug: at("debug"),
           warn: at("warn"), error: at("error") };
})(globalThis["` + hostFn + `"]);
delete globalThis["` + hostFn + `"];
`
