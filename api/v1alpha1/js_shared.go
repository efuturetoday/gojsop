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
// Exactly one of inline, configMapRef, or oci must be set; this is enforced
// by the apiserver via the XValidation rule below.
//
// Embedded by both JSHook.Spec and JSAdmission.Spec.
//
// +kubebuilder:validation:XValidation:rule="(has(self.inline)?1:0) + (has(self.configMapRef)?1:0) + (has(self.oci)?1:0) == 1",message="exactly one of inline, configMapRef, or oci must be set"
type JSSource struct {
	// Inline embeds the JS module text directly into the resource.
	// Convenient for small hooks and demos.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Inline string `json:"inline,omitempty"`

	// ConfigMapRef pulls the JS module text from a key in a ConfigMap.
	// +optional
	ConfigMapRef *ConfigMapKeyRef `json:"configMapRef,omitempty"`

	// OCI pulls the JS module from an OCI artifact (e.g. ghcr.io/foo/hook).
	// The artifact must contain a single layer whose body is the JS source.
	// +optional
	OCI *OCISource `json:"oci,omitempty"`
}

// ConfigMapKeyRef points to a single key inside a ConfigMap.
//
// Both JSHook and JSAdmission are cluster-scoped, so Namespace must be
// specified explicitly — there is no "same namespace" fallback.
type ConfigMapKeyRef struct {
	// Name is the name of the referenced ConfigMap.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the namespace of the referenced ConfigMap.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Key inside the ConfigMap.
	// +kubebuilder:default="hook.js"
	// +kubebuilder:validation:MinLength=1
	// +optional
	Key string `json:"key,omitempty"`
}

// OCISource locates a JS module inside an OCI artifact.
// Exactly one of Tag or Digest must be set.
//
// +kubebuilder:validation:XValidation:rule="(has(self.tag)?1:0) + (has(self.digest)?1:0) == 1",message="exactly one of tag or digest must be set"
type OCISource struct {
	// Repository is the registry + repository path, without a tag or digest
	// (e.g. "ghcr.io/foo/hook").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)+$`
	Repository string `json:"repository"`

	// Tag selects a tagged version (e.g. "v1.2.3"). Mutable; for production
	// pinning prefer Digest.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`
	Tag string `json:"tag,omitempty"`

	// Digest pins a specific manifest digest (e.g. "sha256:abcd..."). Immutable.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[A-Fa-f0-9]{64}$`
	Digest string `json:"digest,omitempty"`
}

// JSResources caps what the persistent JS instance is allowed to consume.
type JSResources struct {
	// MemoryMB is the hard limit on the QuickJS heap in megabytes.
	// +kubebuilder:default=32
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	// +optional
	MemoryMB int32 `json:"memoryMB,omitempty"`

	// TimeoutSeconds bounds a single call into JS.
	// On timeout the call is interrupted; the persistent instance survives
	// unless timeouts repeat (see status.instance.lastRestartReason).
	// +kubebuilder:default=30
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=300
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
	// +kubebuilder:validation:Minimum=0
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
	// +kubebuilder:validation:Minimum=0
	DurationMs int64 `json:"durationMs,omitempty"`
	// Error is non-empty if the last call failed.
	// +optional
	Error string `json:"error,omitempty"`
}
