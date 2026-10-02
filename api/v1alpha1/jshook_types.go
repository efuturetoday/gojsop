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

// JSHookSpec defines the desired state of JSHook.
type JSHookSpec struct {
	// Source is where to load the hook's JS module from.
	// +required
	Source JSSource `json:"source"`

	// Limits caps memory and per-call execution time of the JS instance.
	// +optional
	Limits *JSLimits `json:"limits,omitempty"`

	// Bindings tell the operator which resources to watch and which events
	// call handle(). At least one is required: a hook that watches nothing
	// can never run.
	//
	// Declared here rather than returned by the script, so the apiserver
	// validates them on apply, kubectl shows them without reading the
	// source, and the operator knows the hook's scope before it runs any
	// user code (api-design.R11).
	// +required
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	Bindings []HookBinding `json:"bindings"`
}

// JSHookStatus defines the observed state of JSHook.
type JSHookStatus struct {
	// ObservedGeneration is the generation last reconciled by the controller.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Bindings echoes the watches the operator actually established, one
	// entry per binding and resource, so a user sees the resolved scope
	// without re-deriving it from spec.bindings.
	// +listType=set
	// +optional
	Bindings []string `json:"bindings,omitempty"`

	// Instance reports the prepared script every call starts from.
	// +optional
	Instance *JSInstanceStatus `json:"instance,omitempty"`

	// LastReconcile reports when the reconcile result last changed and what
	// went wrong (if anything). Distinct from runtime call telemetry — a
	// successful reconcile here does NOT mean handle() ran.
	// +optional
	LastReconcile *JSReconcileStatus `json:"lastReconcile,omitempty"`

	// Conditions follows standard Kubernetes condition conventions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=jshook
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].reason`,priority=1
// +kubebuilder:printcolumn:name="LastRestart",type=string,JSONPath=`.status.instance.recentRestarts[0].reason`
// +kubebuilder:printcolumn:name="RestartedAt",type=date,JSONPath=`.status.instance.recentRestarts[0].time`,priority=1
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
