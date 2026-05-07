/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JSSource describes where a JavaScript module body comes from.
// Exactly one of the three fields must be set; the controller validates that.
// Embedded by both JSHook.Spec and JSAdmission.Spec.
type JSSource struct {
	// Inline embeds the JS module text directly into the resource.
	// Convenient for small hooks and demos.
	// +optional
	Inline string `json:"inline,omitempty"`

	// ConfigMapRef pulls the JS module text from a key in a ConfigMap.
	// +optional
	ConfigMapRef *ConfigMapKeyRef `json:"configMapRef,omitempty"`

	// OCIRef pulls the JS module from an OCI artifact (e.g. ghcr.io/foo/hook:v1).
	// The artifact must contain a single layer whose body is the JS source.
	// +optional
	OCIRef string `json:"ociRef,omitempty"`
}

// ConfigMapKeyRef points to a single key inside a ConfigMap.
type ConfigMapKeyRef struct {
	// +required
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Key inside the ConfigMap. Defaults to "hook.js".
	// +optional
	Key string `json:"key,omitempty"`
}

// JSResources caps what the persistent JS instance is allowed to consume.
type JSResources struct {
	// MemoryMB is the hard limit on the QuickJS heap in megabytes. Default 32.
	// +optional
	MemoryMB int32 `json:"memoryMB,omitempty"`

	// TimeoutSeconds bounds a single call into JS. Default 30.
	// On timeout the call is interrupted; the persistent instance survives
	// unless timeouts repeat (see status.instance.lastRestartReason).
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// JSInstanceStatus reports the lifecycle state of the persistent JS instance
// backing a hook or admission policy. Identical shape for both kinds — the
// same Registry produces it. Users rely on this to know whether their
// globalThis state is still alive.
type JSInstanceStatus struct {
	// StartedAt is when the current persistent instance was created.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// SourceHash is a sha256 of the loaded JS source. A change here triggers
	// a controlled instance restart.
	// +optional
	SourceHash string `json:"sourceHash,omitempty"`

	// RestartCount counts how often the persistent instance has been replaced
	// since the resource was created.
	// +optional
	RestartCount int32 `json:"restartCount,omitempty"`

	// LastRestartReason is one of: source-changed, memory-limit, panic,
	// timeout-streak, manual.
	// +optional
	LastRestartReason string `json:"lastRestartReason,omitempty"`

	// ManualRestartToken echoes the value of the gojsop.io/restart annotation
	// that produced the most recent manual restart. Setting the annotation to
	// a new value triggers exactly one restart; re-reconciles with the same
	// value are no-ops.
	// +optional
	ManualRestartToken string `json:"manualRestartToken,omitempty"`
}

// JSExecutionStatus reports the outcome of the most recent JS call (event
// handle() or admission validate()/mutate()).
type JSExecutionStatus struct {
	// +optional
	Time *metav1.Time `json:"time,omitempty"`
	// +optional
	DurationMs int64 `json:"durationMs,omitempty"`
	// Error is non-empty if the last call failed.
	// +optional
	Error string `json:"error,omitempty"`
}
