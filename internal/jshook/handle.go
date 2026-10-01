package jshook

import (
	"context"
	"encoding/json"

	"github.com/o-haase/gojsop/internal/jsrun"
)

// Handle invokes the hook's exported `handle(ctx)` function with a
// BindingContext array on the script of key. Returns the JSON form of
// whatever handle returned (or "" if it returned undefined). The Result
// classifies how the call ended; the error is set only when nothing ran
// (jsrun.ErrUnknownKey, jsrun.ErrVMUnavailable). Locking and panic recovery
// belong to the Runner.
//
// ctx carries the call deadline; the Runner enforces it, so an endless loop
// ends as jsrun.OutcomeCancelled.
// jshook.R7
func Handle(ctx context.Context, rt jsrun.Runner, key jsrun.Key, bindingCtx []BindingContext) (string, jsrun.Result, error) {
	var raw json.RawMessage
	res, err := rt.Invoke(ctx, key, "handle", bindingCtx, &raw)
	return string(raw), res, err
}
