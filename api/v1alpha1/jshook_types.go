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

// JSHookSource describes where the JavaScript module body comes from.
// Exactly one of the three fields must be set; the controller validates that.
type JSHookSource struct {
	// Inline embeds the JS module text directly into the JSHook resource.
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

// JSHookResources caps what the persistent JS instance is allowed to consume.
type JSHookResources struct {
	// MemoryMB is the hard limit on the QuickJS heap in megabytes. Default 32.
	// +optional
	MemoryMB int32 `json:"memoryMB,omitempty"`

	// TimeoutSeconds bounds a single handle() call. Default 30.
	// On timeout the call is interrupted; the persistent instance survives
	// unless timeouts repeat (see status.instance.lastRestartReason).
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// JSHookSpec defines the desired state of JSHook.
type JSHookSpec struct {
	// Source is where to load the hook's JS module from.
	// +required
	Source JSHookSource `json:"source"`

	// Resources caps memory and per-call execution time of the JS instance.
	// +optional
	Resources *JSHookResources `json:"resources,omitempty"`
}

// JSHookInstanceStatus reports the lifecycle state of the persistent
// JavaScript instance backing this hook. Users rely on this to know whether
// their globalThis state is still alive.
type JSHookInstanceStatus struct {
	// StartedAt is when the current persistent instance was created.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// SourceHash is a sha256 of the loaded JS source. A change here triggers
	// a controlled instance restart.
	// +optional
	SourceHash string `json:"sourceHash,omitempty"`

	// RestartCount counts how often the persistent instance has been replaced
	// since the JSHook was created.
	// +optional
	RestartCount int32 `json:"restartCount,omitempty"`

	// LastRestartReason is one of: source-changed, memory-limit, panic,
	// timeout-streak, manual.
	// +optional
	LastRestartReason string `json:"lastRestartReason,omitempty"`
}

// JSHookExecutionStatus reports the outcome of the most recent handle() call.
type JSHookExecutionStatus struct {
	// +optional
	Time *metav1.Time `json:"time,omitempty"`
	// +optional
	DurationMs int64 `json:"durationMs,omitempty"`
	// Error is non-empty if the last call failed.
	// +optional
	Error string `json:"error,omitempty"`
}

// JSHookStatus defines the observed state of JSHook.
type JSHookStatus struct {
	// Phase is a coarse rollup: Pending, Ready, Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// ObservedGeneration is the generation last reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Bindings are the resolved bindings the hook declared in its config()
	// call (kubernetes/schedule/onStartup), echoed here so users can see what
	// the hook is subscribed to.
	// +optional
	Bindings []string `json:"bindings,omitempty"`

	// Instance reports the lifecycle of the persistent JS runtime.
	// +optional
	Instance *JSHookInstanceStatus `json:"instance,omitempty"`

	// LastExecution reports the most recent handle() call.
	// +optional
	LastExecution *JSHookExecutionStatus `json:"lastExecution,omitempty"`

	// Conditions follows standard Kubernetes condition conventions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=jshook
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Bindings",type=integer,JSONPath=`.status.bindings[*]`,priority=1
// +kubebuilder:printcolumn:name="Restarts",type=integer,JSONPath=`.status.instance.restartCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// JSHook declares a JavaScript-based Kubernetes hook. The controller loads
// the module, calls its config() export to learn which events to subscribe to,
// then dispatches matching events to its handle() export.
//
// JSHook is cluster-scoped because hooks typically observe resources across
// namespaces.
type JSHook struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of JSHook
	// +required
	Spec JSHookSpec `json:"spec"`

	// status defines the observed state of JSHook
	// +optional
	Status JSHookStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// JSHookList contains a list of JSHook
type JSHookList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []JSHook `json:"items"`
}

func init() {
	SchemeBuilder.Register(&JSHook{}, &JSHookList{})
}
