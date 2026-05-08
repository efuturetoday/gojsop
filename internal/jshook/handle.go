package jshook

import "github.com/o-haase/gojsop/internal/jsengine"

// Handle invokes the hook's exported `handle(ctx)` function with a
// BindingContext array. Returns the JSON form of whatever handle returned
// (or "" if it returned undefined). Errors from inside JS surface as Go
// errors. Caller MUST hold the per-VM serialization lock — qjs is not
// goroutine-safe.
func Handle(vm *jsengine.VM, bindingCtx []BindingContext) (string, error) {
	return vm.CallExport("handle", bindingCtx)
}
