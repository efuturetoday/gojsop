package jshook

import (
	"context"
	"encoding/json"

	"github.com/o-haase/gojsop/internal/jsrun"
)

// Handle calls the hook's handle(event) with ev on the script of key, through
// EntryPoint. Returns the JSON form of whatever handle returned (or "" if it
// returned undefined). The Result classifies how the call ended; the error is
// set only when nothing ran (jsrun.ErrUnknownKey, jsrun.ErrVMUnavailable).
// Locking and panic recovery belong to the Runner.
//
// ctx carries the call deadline; the Runner enforces it, so an endless loop
// ends as jsrun.OutcomeCancelled. Install a Lister with WithLister for
// event.all().
// jshook.R7
func Handle(ctx context.Context, rt jsrun.Runner, key jsrun.Key, ev Event) (string, jsrun.Result, error) {
	var raw json.RawMessage
	res, err := rt.Invoke(ctx, key, EntryPoint, ev, &raw)
	return string(raw), res, err
}
