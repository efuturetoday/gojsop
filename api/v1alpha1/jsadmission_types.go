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

// AdmissionRule mirrors a single rule entry inside a *WebhookConfiguration's
// webhooks[].rules[]. The reconciler pastes these straight through into the
// aggregated central VWC/MWC.
type AdmissionRule struct {
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	APIGroups []string `json:"apiGroups"`

	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	APIVersions []string `json:"apiVersions"`

	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	Resources []string `json:"resources"`

	// Operations the apiserver should send.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Enum=CREATE;UPDATE;DELETE;CONNECT;*
	Operations []string `json:"operations"`

	// Scope is one of "*" (default), "Namespaced", "Cluster".
	// +optional
	// +kubebuilder:validation:Enum="*";Namespaced;Cluster
	Scope string `json:"scope,omitempty"`
}

// JSAdmissionSpec defines the desired state of a JSAdmission policy.
type JSAdmissionSpec struct {
	// Source is where to load the policy's JS module from.
	// +required
	Source JSSource `json:"source"`

	// Resources caps memory and per-call execution time of the JS instance.
	// +optional
	Resources *JSResources `json:"resources,omitempty"`

	// Type selects the webhook flavour. validating policies must export
	// validate(req); mutating policies must export mutate(req) and may
	// return a modifiedObject from which the controller computes a JSONPatch.
	// +kubebuilder:validation:Enum=validating;mutating
	// +kubebuilder:default=validating
	// +optional
	Type string `json:"type,omitempty"`

	// Rules tell the apiserver which resources to forward to this policy.
	// At least one rule is required.
	// +required
	// +kubebuilder:validation:MinItems=1
	Rules []AdmissionRule `json:"rules"`

	// FailurePolicy controls what happens if the JS handler errors, panics,
	// or times out. Fail rejects the request; Ignore lets it through.
	// +kubebuilder:validation:Enum=Fail;Ignore
	// +kubebuilder:default=Fail
	// +optional
	FailurePolicy string `json:"failurePolicy,omitempty"`

	// MatchPolicy is forwarded to admissionregistration.k8s.io/v1.
	// +kubebuilder:validation:Enum=Exact;Equivalent
	// +kubebuilder:default=Equivalent
	// +optional
	MatchPolicy string `json:"matchPolicy,omitempty"`

	// NamespaceSelector restricts which namespaces are subject to this
	// policy. Forwarded to webhooks[].namespaceSelector.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// ObjectSelector restricts which objects are subject to this policy.
	// +optional
	ObjectSelector *metav1.LabelSelector `json:"objectSelector,omitempty"`

	// TimeoutSeconds bounds a single admission review call. The apiserver
	// receives this as webhooks[].timeoutSeconds; our HTTP handler wraps the
	// JS call in a context with the same deadline.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// SideEffects is required by admissionregistration/v1. None is the safe
	// default for pure-Go/JS policies.
	// +kubebuilder:validation:Enum=None;NoneOnDryRun;Some;Unknown
	// +kubebuilder:default=None
	// +optional
	SideEffects string `json:"sideEffects,omitempty"`
}

// JSAdmissionStatus reports the observed state of a JSAdmission policy.
type JSAdmissionStatus struct {
	// ObservedGeneration is the generation last reconciled by the controller.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// WebhookPath is the HTTP path the operator's webhook server registered
	// for this policy (e.g. /admission/validate/default/prevent-latest-tags).
	// +optional
	WebhookPath string `json:"webhookPath,omitempty"`

	// WebhookConfigName is the name of the central *WebhookConfiguration
	// (gojsop-validating or gojsop-mutating) that holds this policy's
	// webhooks[] entry. Observable, not configurable.
	// +optional
	WebhookConfigName string `json:"webhookConfigName,omitempty"`

	// Instance reports the lifecycle of the persistent JS runtime.
	// +optional
	Instance *JSInstanceStatus `json:"instance,omitempty"`

	// LastReview reports the most recent validate()/mutate() call.
	// +optional
	LastReview *JSExecutionStatus `json:"lastReview,omitempty"`

	// Conditions follows standard Kubernetes condition conventions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=jsadm
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].reason`,priority=1
// +kubebuilder:printcolumn:name="Restarts",type=integer,JSONPath=`.status.instance.restartCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// JSAdmission declares a JavaScript-backed admission webhook policy. The
// controller loads the module, registers an HTTP handler on the operator's
// webhook server, and adds an entry to the cluster's central VWC/MWC.
//
// JSAdmission is cluster-scoped because admission policies are inherently
// cluster-wide.
type JSAdmission struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of JSAdmission
	// +required
	Spec JSAdmissionSpec `json:"spec"`

	// status defines the observed state of JSAdmission
	// +optional
	Status JSAdmissionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// JSAdmissionList contains a list of JSAdmission
type JSAdmissionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []JSAdmission `json:"items"`
}

func init() {
	SchemeBuilder.Register(&JSAdmission{}, &JSAdmissionList{})
}
