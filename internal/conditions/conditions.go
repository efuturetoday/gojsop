// Package conditions holds the status-condition vocabulary and shared
// annotations used by both the JSHook and JSAdmission reconcilers.
package conditions

import (
	"errors"

	"github.com/o-haase/gojsop/internal/jsregistry"
)

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

// Event reasons recorded into corev1.Event so kubectl describe surfaces a
// chronological lifecycle trail. Messages must be drawn from a small, finite
// set per Reason — the recorder dedupes on (Reason, Message) exact match,
// so per-request detail (UIDs, names, error strings) MUST stay out of the
// Message and live in conditions/status/logs instead.
const (
	// Reconciler lifecycle
	EventReconciled        = "Reconciled"
	EventRestarted         = "Restarted"
	EventSourceLoadFailed  = "SourceLoadFailed"
	EventBuildFailed       = "BuildFailed"
	EventModuleLoadFailed  = "ModuleLoadFailed"
	EventConfigInvalid     = "ConfigInvalid"
	EventEntrypointMissing = "EntrypointMissing"
	EventSubscribeFailed   = "SubscribeFailed"
	EventWebhookRegistered = "WebhookRegistered"

	// Dispatcher rescue / handle()
	EventRescueFailed  = "RescueFailed"
	EventHandleFailed  = "HandleFailed"
	EventHandleTimeout = "HandleTimeout"

	// Admission review
	EventReviewPanicked = "ReviewPanicked"
	EventReviewFailed   = "ReviewFailed"
	EventReviewTimeout  = "ReviewTimeout"
)

// ClassifyBuildError maps a registry build error to a stable Event reason
// and a low-cardinality Message template. Uses typed sentinels from
// jsregistry (ErrNewVM, ErrBindHost, ErrLoadModule, ErrPostBuild) plus the
// MissingExportError type — never sniffs error message strings.
//
// Shared between JSHook and JSAdmission reconcilers because both go through
// the same Registry.GetOrLoad/RestartByKey path.
// status-conditions.R3
func ClassifyBuildError(err error) (reason, message string) {
	var miss *jsregistry.MissingExportError
	if errors.As(err, &miss) {
		return EventEntrypointMissing, miss.Error()
	}
	switch {
	case errors.Is(err, jsregistry.ErrLoadModule):
		return EventModuleLoadFailed, "module load failed"
	case errors.Is(err, jsregistry.ErrPostBuild):
		return EventConfigInvalid, "config() returned an error"
	case errors.Is(err, jsregistry.ErrBindHost):
		return EventBuildFailed, "build failed: bind host"
	case errors.Is(err, jsregistry.ErrNewVM):
		return EventBuildFailed, "build failed: new vm"
	default:
		return EventBuildFailed, "build failed: unknown"
	}
}
