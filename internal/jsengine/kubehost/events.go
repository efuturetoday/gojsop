package kubehost

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/efuturetoday/gojsop/internal/jsengine"
	"github.com/efuturetoday/gojsop/internal/jshook"
)

// allFn is the host function behind event.all().
const allFn = "__gojsop_all"

// HookEvents binds the event API of a hook: jshook.EntryPoint calls the
// author's handle(event) and gives event an all() that returns the objects
// the event's binding watches right now, from the watch's cache, without a
// call to the apiserver.
// jshook.R26
type HookEvents struct{}

// Bind implements jsengine.HostBinder.
func (HookEvents) Bind(h *jsengine.Host) error {
	h.Func(allFn, all)
	h.Shim(eventShim)
	return nil
}

func all(ctx context.Context, _ json.RawMessage) (any, error) {
	l := jshook.ListerFrom(ctx)
	if l == nil {
		return nil, errors.New("event.all() is only available while handle() runs")
	}
	objs, err := l()
	if err != nil {
		return nil, err
	}
	if objs == nil {
		objs = []map[string]any{}
	}
	return objs, nil
}

// eventShim defines jshook.EntryPoint. It looks handle up when the event
// arrives, so it finds the function the module defined after the shim ran,
// and it hides the raw host function from the script.
const eventShim = `
globalThis["` + jshook.EntryPoint + `"] = (function (all) {
  return function (event) {
    event.all = function () { return all(null); };
    return handle(event);
  };
})(globalThis["` + allFn + `"]);
delete globalThis["` + allFn + `"];
`
