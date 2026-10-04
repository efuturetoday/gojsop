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
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JSSource describes where a JavaScript module body comes from.
// Exactly one of inline or configMapRef must be set; this is enforced
// by the apiserver via the XValidation rule below.
//
// Embedded by both JSHook.Spec and JSAdmission.Spec.
//
// +kubebuilder:validation:XValidation:rule="(has(self.inline)?1:0) + (has(self.configMapRef)?1:0) == 1",message="exactly one of inline or configMapRef must be set"
type JSSource struct {
	// Inline embeds the JS module text directly into the resource.
	// Convenient for small hooks and demos.
	// At most 512 KiB; larger scripts belong in a ConfigMap.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=524288
	Inline string `json:"inline,omitempty"`

	// ConfigMapRef pulls the JS module text from a key in a ConfigMap.
	// +optional
	ConfigMapRef *ConfigMapKeyRef `json:"configMapRef,omitempty"`
}

// ConfigMapKeyRef points to a single key inside a ConfigMap.
//
// Both JSHook and JSAdmission are cluster-scoped, so Namespace must be
// specified explicitly — there is no "same namespace" fallback.
type ConfigMapKeyRef struct {
	// Name is the name of the referenced ConfigMap.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace is the namespace of the referenced ConfigMap.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`

	// Key inside the ConfigMap.
	// +kubebuilder:default="hook.js"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Key string `json:"key,omitempty"`
}

// JSLimits caps what one call into the script may consume. Every call runs on
// its own instance restored from the prepared script.
type JSLimits struct {
	// MemoryMB is the hard limit on the QuickJS heap of one call in megabytes.
	// +kubebuilder:default=32
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	// +optional
	MemoryMB int32 `json:"memoryMB,omitempty"`

	// TimeoutSeconds bounds a single call into JS, and the build (module load
	// and top-level code). On timeout the call is stopped; the next call starts
	// from the prepared script.
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// JSRestartEvent records one time the operator prepared the script of a hook
// or admission policy again. Mirrored from the registry's log so users can
// see the recent trail without `kubectl logs` on the operator.
type JSRestartEvent struct {
	// Time is when the newly prepared script was installed.
	// +optional
	Time *metav1.Time `json:"time,omitempty"`

	// Reason is what made the operator prepare the script again: the source
	// changed, the limits changed, or the gojsop.io/restart annotation got a
	// new value.
	// +kubebuilder:validation:Enum=source-changed;limits-changed;manual
	// +optional
	Reason string `json:"reason,omitempty"`

	// Error is a diagnostic recorded with the event, if any.
	// +optional
	Error string `json:"error,omitempty"`
}

// JSScriptStatus reports the prepared script of a hook or admission policy.
// Identical shape for both kinds. Every call starts from this prepared
// script, so no state survives between calls.
type JSScriptStatus struct {
	// PreparedAt is when the current script was prepared.
	// +optional
	PreparedAt *metav1.Time `json:"preparedAt,omitempty"`

	// SourceHash is a sha256 of the loaded JS source. A change here prepares
	// the script again.
	// +optional
	SourceHash string `json:"sourceHash,omitempty"`

	// RestartsByReason aggregates RecentRestarts. Keys are the reasons of
	// RecentRestarts; values are counts since the operator started.
	// +optional
	RestartsByReason map[string]int32 `json:"restartsByReason,omitempty"`

	// RecentRestarts is the most-recent-first restart log, capped at 20
	// entries. recentRestarts[0] is the newest event so JSONPath print
	// columns can show the latest reason without index-from-end gymnastics.
	// +optional
	// +listType=atomic
	RecentRestarts []JSRestartEvent `json:"recentRestarts,omitempty"`

	// RestartToken echoes the value of the gojsop.io/restart annotation
	// that produced the most recent manual restart. Setting the annotation to
	// a new value triggers exactly one restart; re-reconciles with the same
	// value are no-ops.
	// +optional
	RestartToken string `json:"restartToken,omitempty"`
}

// JSReconcileStatus reports the outcome of the most recent change of the
// reconcile result for the resource. Distinct from runtime call telemetry —
// this records whether the controller could *load and register* the
// hook/policy, not whether handle()/validate()/mutate() actually ran.
type JSReconcileStatus struct {
	// Time is when the reconcile result last changed: a failure, or the first
	// success after a failure.
	// +optional
	Time *metav1.Time `json:"time,omitempty"`
	// Error is non-empty when the last reconcile failed (source load,
	// build, subscribe, webhook sync). Empty on a successful reconcile.
	// +optional
	Error string `json:"error,omitempty"`
}

// ResourceRule selects a set of API resources. Both kinds name resources the
// same way: a JSAdmission rule and a JSHook binding differ in what triggers
// them (an admission request vs. a watch event), not in what they point at.
//
// JSAdmission forwards these fields to the apiserver, which resolves "*"
// itself. A JSHook turns them into informers, so it needs concrete values:
// the cross product of apiGroups x apiVersions x resources must resolve, and
// "*" is rejected (api-design.R10).
type ResourceRule struct {
	// APIGroups is the list of API groups, "" for the core group.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	APIGroups []string `json:"apiGroups"`

	// APIVersions is the list of versions, e.g. ["v1"].
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	APIVersions []string `json:"apiVersions"`

	// Resources is the list of plural resource names, e.g. ["configmaps"].
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	Resources []string `json:"resources"`

	// Scope is one of "*" (default), "Namespaced", "Cluster".
	// +optional
	// +kubebuilder:validation:Enum="*";Namespaced;Cluster
	Scope string `json:"scope,omitempty"`
}

// ObjectMatch narrows which objects a rule or binding applies to. Identical
// in meaning for both kinds: JSAdmission forwards it to the webhook
// configuration, a JSHook applies it to its informers.
//
// To select a single namespace by name, match the label every namespace
// carries since Kubernetes 1.21:
//
//	namespaceSelector:
//	  matchLabels:
//	    kubernetes.io/metadata.name: my-namespace
type ObjectMatch struct {
	// NamespaceSelector matches the labels of the object's namespace.
	// A cluster-scoped object has no namespace and therefore matches only
	// when the selector is empty.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// ObjectSelector matches the labels of the object itself.
	// +optional
	ObjectSelector *metav1.LabelSelector `json:"objectSelector,omitempty"`
}

// HookEvent is a watch event that triggers a JSHook binding.
// +kubebuilder:validation:Enum=Added;Modified;Deleted
type HookEvent string

const (
	HookEventAdded    HookEvent = "Added"
	HookEventModified HookEvent = "Modified"
	HookEventDeleted  HookEvent = "Deleted"
)

// HookBinding is one slice of the cluster a JSHook reacts to. It is the
// counterpart of AdmissionRule: same resource selection, same object
// matching, a different trigger.
type HookBinding struct {
	// Name identifies this binding in the binding context handed to
	// handle(), in status.bindings and in events. Must be unique within the
	// hook.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// ResourceRule selects which resources to watch.
	ResourceRule `json:",inline"`

	// ObjectMatch narrows which objects of those resources to react to.
	ObjectMatch `json:",inline"`

	// Events selects which watch events call handle(). Defaults to all three.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=3
	// +kubebuilder:default={Added,Modified,Deleted}
	Events []HookEvent `json:"events,omitempty"`

	// Synchronization asks for one initial call carrying every object that
	// already exists, before any event is delivered. With it disabled those
	// objects arrive as Added events instead.
	// +optional
	// +kubebuilder:default=true
	Synchronization *bool `json:"synchronization,omitempty"`
}

// WantsEvent reports whether this binding asked for the given watch event.
func (b *HookBinding) WantsEvent(e HookEvent) bool {
	if len(b.Events) == 0 {
		return true
	}
	return slices.Contains(b.Events, e)
}

// WantsSynchronization reports whether the initial snapshot call is enabled.
func (b *HookBinding) WantsSynchronization() bool {
	return b.Synchronization == nil || *b.Synchronization
}

// PermissionVerb is a verb a Permission grants.
// +kubebuilder:validation:Enum=get;list;watch;create;update;patch;delete
type PermissionVerb string

// Permission grants a script one set of rights: the verbs on the resources
// named, in every namespace. It has the shape of an RBAC rule without
// wildcards, so it says exactly what the script may touch.
type Permission struct {
	// APIGroups is the list of API groups, "" for the core group.
	// "*" is not allowed.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9.-]*$`
	APIGroups []string `json:"apiGroups"`

	// Resources is the list of plural resource names, e.g. ["configmaps"],
	// or a subresource such as "deployments/scale". "*" is not allowed.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9.-]+(/[a-z0-9.-]+)?$`
	Resources []string `json:"resources"`

	// Verbs is the list of verbs granted on those resources.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	Verbs []PermissionVerb `json:"verbs"`
}
