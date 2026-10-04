package jshook

import "context"

// Event is what a hook's handle(event) receives: one change of one object
// that one binding watches. The script also gets event.all(), the objects
// the binding watches right now (kubehost binds it, Lister feeds it).
// jshook.R7
type Event struct {
	// Binding is the name of the binding that watches the object.
	Binding string `json:"binding"`
	// Type is "Added", "Modified" or "Deleted".
	Type string `json:"type"`
	// Object is the object as the watch saw it; for Deleted its last state.
	Object map[string]any `json:"object"`
	// Initial marks an Added for an object that already existed when the
	// watch started.
	Initial bool `json:"initial"`
}

// EntryPoint is the export Handle calls. The shim kubehost binds defines it
// around the author's handle() and adds event.all().
const EntryPoint = "__gojsop_handle"

// Lister returns the objects of the binding of the running call, as the
// watch currently holds them.
type Lister func() ([]map[string]any, error)

type listerKey struct{}

// WithLister returns a context whose event.all() answers from l. The
// dispatcher installs it on the context of every call.
func WithLister(ctx context.Context, l Lister) context.Context {
	return context.WithValue(ctx, listerKey{}, l)
}

// ListerFrom returns the Lister of this call, or nil when none was installed.
func ListerFrom(ctx context.Context) Lister {
	l, _ := ctx.Value(listerKey{}).(Lister)
	return l
}
