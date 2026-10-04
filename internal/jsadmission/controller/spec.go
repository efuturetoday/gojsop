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

package controller

import (
	"context"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/jsrun"
)

// This file holds what both JSAdmission reconcilers derive from one policy.
// The server reconciler runs on every replica and builds the script; the
// status reconciler runs on the leader and reports it (jsadmission.R20).
// Both have to read the same spec the same way, so the derivation lives here
// once instead of in each of them.

// isMutating reports whether pol is a mutating policy.
func isMutating(pol *corev1alpha1.JSAdmission) bool {
	return pol.Spec.Type == "mutating"
}

// failurePolicyOf is the failurePolicy gojsop applies: Ignore under
// enforcement Warn or Audit, which never deny, else the spec's.
// jsadmission.R28
func failurePolicyOf(pol *corev1alpha1.JSAdmission) admissionregv1.FailurePolicyType {
	switch pol.Spec.Enforcement {
	case corev1alpha1.EnforcementWarn, corev1alpha1.EnforcementAudit:
		return admissionregv1.Ignore
	}
	return admissionregv1.FailurePolicyType(pol.Spec.FailurePolicy)
}

// admissionLimitsFromSpec maps the CRD's optional Limits to jsrun.Limits.
func admissionLimitsFromSpec(r *corev1alpha1.JSLimits) jsrun.Limits {
	if r == nil {
		return jsrun.Limits{}
	}
	return jsrun.Limits{MemoryMB: r.MemoryMB, TimeoutSeconds: r.TimeoutSeconds}
}

// admissionPostBuild returns a PostBuild closure that asserts the loaded
// module exposes the entrypoint required by the policy's spec.type
// (validate for validating, mutate for mutating). A missing entrypoint is
// surfaced as a typed MissingExportError so the reconciler can map it to
// the EntrypointMissing event reason without sniffing message strings.
func admissionPostBuild(mutating bool) jsrun.PostBuildHook {
	entry := "validate"
	if mutating {
		entry = "mutate"
	}
	return func(ctx context.Context, s jsrun.Script) error {
		_ = ctx
		if !s.HasExport(entry) {
			return &jsrun.MissingExportError{Name: entry}
		}
		return nil
	}
}

// callTimeout is the deadline of one review: the smaller of the webhook
// timeout (spec.timeoutSeconds, default 5) and the call limit
// (spec.limits.timeoutSeconds, default 30). The runner applies the limit on
// its own; the server needs the smaller value to report a timeout correctly.
// jsadmission.R11
func callTimeout(webhookSeconds int32, lim jsrun.Limits) time.Duration {
	s := min(orInt32(webhookSeconds, 5), lim.WithDefaults().TimeoutSeconds)
	return time.Duration(s) * time.Second
}

func orInt32(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}
