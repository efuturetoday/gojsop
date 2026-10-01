package jshook

import (
	"context"

	"github.com/o-haase/gojsop/internal/jsengine"
)

// Handle invokes the hook's exported `handle(ctx)` function with a
// BindingContext array. Returns the JSON form of whatever handle returned
// (or "" if it returned undefined). Errors from inside JS surface as Go
// errors. Caller MUST hold the per-VM serialization lock — qjs is not
// goroutine-safe.
//
// ctx is plumbed into wazero via the VM's CloseOnContextDone wiring;
// passing a context with deadline gives the JS call a real timeout
// (returns jsengine.ErrCancelled).
// Block: hook-dispatch R5
func Handle(ctx context.Context, vm *jsengine.VM, bindingCtx []BindingContext) (string, error) {
	return vm.CallExport(ctx, "handle", bindingCtx)
}
