package jshook

import "encoding/json"

// BindingContext is the per-event payload shipped into a hook's handle(ctx).
// Schema mirrors flant/shell-operator so authors familiar with that project
// have the same mental model.
//
// For Synchronization-type contexts, Objects is populated and Object/WatchEvent
// are zero. For Event-type contexts, Object/WatchEvent are populated.
type BindingContext struct {
	Binding    string         `json:"binding"`
	Type       string         `json:"type"`                 // "Synchronization" | "Event"
	WatchEvent string         `json:"watchEvent,omitempty"` // "Added" | "Modified" | "Deleted"
	Object     map[string]any `json:"object,omitempty"`     // event-type only
	Objects    []SyncObject   `json:"objects,omitempty"`    // sync-type only
}

// SyncObject is one entry in a Synchronization binding-context payload.
type SyncObject struct {
	Object map[string]any `json:"object"`
}

// TypeSynchronization is the Type of the context that carries every object
// a binding matched when its watch started.
const TypeSynchronization = "Synchronization"

// MarshalJSON writes objects as [] on a Synchronization that matched
// nothing, so a script can always read ctx.objects.length.
// jshook.R7
func (c BindingContext) MarshalJSON() ([]byte, error) {
	type plain BindingContext
	if c.Type == TypeSynchronization && len(c.Objects) == 0 {
		return json.Marshal(struct {
			plain
			Objects []SyncObject `json:"objects"`
		}{plain(c), []SyncObject{}})
	}
	return json.Marshal(plain(c))
}
