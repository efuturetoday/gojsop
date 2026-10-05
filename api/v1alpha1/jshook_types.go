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
	"k8s.io/apimachinery/pkg/runtime"
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
	// +kubebuilder:validation:MaxItems=32
	Bindings []HookBinding `json:"bindings"`

	// Permissions are the rights the script has on the cluster through
	// kube.*. gojsop gives the hook a ServiceAccount of its own with exactly
	// these rights, plus get, list and watch on every resource a binding
	// watches; the watches and every kube.* call run as that ServiceAccount.
	// Whoever creates or changes the hook must hold all of these rights.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=64
	Permissions []Permission `json:"permissions,omitempty"`
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

	// ServiceAccount is the ServiceAccount, in the operator's namespace,
	// that the hook's watches and kube.* calls run as.
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// Script reports the prepared script every call starts from.
	// +optional
	Script *JSScriptStatus `json:"script,omitempty"`

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
// +kubebuilder:printcolumn:name="LastRestart",type=string,JSONPath=`.status.script.recentRestarts[0].reason`
// +kubebuilder:printcolumn:name="RestartedAt",type=date,JSONPath=`.status.script.recentRestarts[0].time`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// JSHook declares a JavaScript-based Kubernetes hook. The controller watches
// the resources named in spec.bindings and calls the script's handle() with
// every matching change.
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
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &JSHook{}, &JSHookList{})
		return nil
	})
}
