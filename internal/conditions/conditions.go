// Package conditions holds the status-condition vocabulary and shared
// annotations used by both the JSHook and JSAdmission reconcilers.
package conditions

const (
	// Ready is set on every reconciled CR; True when the persistent JS instance
	// is loaded and (for JSHook) bindings are subscribed.
	Ready = "Ready"

	// ReasonReconciled is the Reason on a healthy Ready=True condition.
	ReasonReconciled = "Reconciled"

	// ReasonFailed is the Reason on a Ready=False condition surfaced by the
	// reconciler when source loading, instance startup, or subscription fails.
	ReasonFailed = "Failed"

	// ManualRestartAnnotation triggers a manual instance restart on either
	// CR. A new annotation value (typically a timestamp) forces exactly one
	// rebuild; the same value on later reconciles is a no-op.
	ManualRestartAnnotation = "gojsop.io/restart"
)
