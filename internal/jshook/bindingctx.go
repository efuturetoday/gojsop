package jshook

// BindingContext is the per-event payload shipped into a hook's handle(ctx).
// Schema mirrors flant/shell-operator so authors familiar with that project
// have the same mental model.
//
// For Synchronization-type contexts, Objects is populated and Object/WatchEvent
// are zero. For Event-type contexts, Object/WatchEvent are populated.
type BindingContext struct {
	Binding      string         `json:"binding"`
	Type         string         `json:"type"`                   // "Synchronization" | "Event" | "Schedule"
	WatchEvent   string         `json:"watchEvent,omitempty"`   // "Added" | "Modified" | "Deleted"
	Object       map[string]any `json:"object,omitempty"`       // event-type only
	FilterResult any            `json:"filterResult,omitempty"` // jqFilter output, when set
	Objects      []SyncObject   `json:"objects,omitempty"`      // sync-type only
	Snapshots    map[string]any `json:"snapshots,omitempty"`    // includeSnapshotsFrom, when wired
}

// SyncObject is one entry in a Synchronization binding-context payload.
type SyncObject struct {
	Object       map[string]any `json:"object"`
	FilterResult any            `json:"filterResult,omitempty"`
}
