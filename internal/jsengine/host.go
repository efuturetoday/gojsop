package jsengine

import "github.com/fastschema/qjs"

// HostBinder registers host-side bindings (Go functions/objects exposed to JS)
// onto a fresh qjs.Context. Implementations should be idempotent and not
// retain references to the context after Bind returns.
type HostBinder interface {
	Bind(ctx *qjs.Context) error
}

// HostBinderFunc adapts a plain function to the HostBinder interface.
type HostBinderFunc func(*qjs.Context) error

func (f HostBinderFunc) Bind(ctx *qjs.Context) error { return f(ctx) }

// BindHost runs the binder against this VM's context. Call before LoadModule
// so user code sees the host objects on globalThis.
func (vm *VM) BindHost(b HostBinder) error {
	if b == nil {
		return nil
	}
	return b.Bind(vm.rt.Context())
}
