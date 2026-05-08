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
	"fmt"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsadmission"
	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jssource"
)

// JSAdmissionReconciler reconciles a JSAdmission object.
//
// Each JSAdmission is backed by exactly one persistent QuickJS instance held
// in the same Registry the JSHookReconciler uses, plus a webhooks[] entry in
// one of the two central WebhookConfigurations gojsop owns.
type JSAdmissionReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Loader resolves spec.source to JS bytes (defaults to inline-only).
	Loader *jssource.Chain
	// Registry owns the per-policy persistent JS instances.
	Registry *jsregistry.Registry
	// Server holds the live policy table the HTTP webhook handler consults.
	Server *jsadmission.Server
	// Registrar maintains the central VWC/MWC.
	Registrar *jsadmission.Registrar
}

// +kubebuilder:rbac:groups=core.gojsop.io,resources=jsadmissions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jsadmissions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jsadmissions/finalizers,verbs=update
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations;mutatingwebhookconfigurations,verbs=get;list;watch;create;update;patch;delete

func (r *JSAdmissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("jsadmission", req.Name)

	var pol corev1alpha1.JSAdmission
	if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
		if apierrors.IsNotFound(err) {
			if r.Server != nil {
				r.Server.Unregister(req.NamespacedName)
			}
			if r.Registrar != nil {
				r.Registrar.Remove(req.NamespacedName)
			}
			r.Registry.Drop(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	source, err := r.Loader.Load(ctx, pol.Spec.Source)
	if err != nil {
		log.Error(err, "loading admission source")
		return r.failAdmission(ctx, &pol, fmt.Sprintf("source: %v", err))
	}
	srcHash := jssource.Hash(source)
	lim := admissionLimitsFromSpec(pol.Spec.Resources)

	mi, restarted, err := r.Registry.GetOrLoad(req.NamespacedName, source, srcHash, lim, nil)
	if err != nil {
		log.Error(err, "registry GetOrLoad")
		return r.failAdmission(ctx, &pol, fmt.Sprintf("instance: %v", err))
	}
	if restarted {
		log.Info("instance (re)started", "hash", srcHash[:12], "restarts", mi.RestartCount, "reason", mi.LastReason)
	}

	// Manual-restart annotation, mirrors the JSHook flow.
	if token, ok := pol.GetAnnotations()[conditions.ManualRestartAnnotation]; ok && !restarted {
		var prev string
		if pol.Status.Instance != nil {
			prev = pol.Status.Instance.ManualRestartToken
		}
		if token != prev {
			newMI, err := r.Registry.RestartByKey(req.NamespacedName, jsregistry.ReasonManual, nil)
			if err != nil {
				log.Error(err, "manual restart")
				return r.failAdmission(ctx, &pol, fmt.Sprintf("manual restart: %v", err))
			}
			log.Info("manual restart applied", "token", token, "restarts", newMI.RestartCount)
			mi = newMI
		}
	}

	mutating := pol.Spec.Type == "mutating"
	path := jsadmission.PathFor(req.NamespacedName, mutating)
	timeout := time.Duration(orInt32(pol.Spec.TimeoutSeconds, 5)) * time.Second

	// Publish to the HTTP server first — once the central VWC/MWC points at us,
	// requests start arriving and a missing entry would 404 under FailurePolicy.
	if r.Server != nil {
		r.Server.Register(jsadmission.PolicyEntry{
			Key:           req.NamespacedName,
			Mutating:      mutating,
			Timeout:       timeout,
			FailurePolicy: admissionregv1.FailurePolicyType(pol.Spec.FailurePolicy),
		})
	}

	if r.Registrar != nil {
		apiRules := make([]jsadmission.APIRule, 0, len(pol.Spec.Rules))
		for _, rule := range pol.Spec.Rules {
			apiRules = append(apiRules, jsadmission.APIRule{
				APIGroups:   rule.APIGroups,
				APIVersions: rule.APIVersions,
				Resources:   rule.Resources,
				Operations:  rule.Operations,
				Scope:       rule.Scope,
			})
		}
		r.Registrar.Upsert(jsadmission.PolicyMeta{
			Key:            req.NamespacedName,
			Path:           path,
			Mutating:       mutating,
			Rules:          jsadmission.RulesFromAPI(apiRules),
			FailurePolicy:  admissionregv1.FailurePolicyType(pol.Spec.FailurePolicy),
			MatchPolicy:    admissionregv1.MatchPolicyType(pol.Spec.MatchPolicy),
			SideEffects:    admissionregv1.SideEffectClass(pol.Spec.SideEffects),
			TimeoutSeconds: pol.Spec.TimeoutSeconds,
			NSSelector:     pol.Spec.NamespaceSelector,
			ObjectSelector: pol.Spec.ObjectSelector,
		})
	}

	startedAt := metav1.NewTime(mi.StartedAt)
	apimeta.SetStatusCondition(&pol.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReconciled,
		Message:            "policy registered",
		ObservedGeneration: pol.Generation,
	})
	pol.Status.ObservedGeneration = pol.Generation
	pol.Status.WebhookPath = path
	if mutating {
		pol.Status.WebhookConfigName = jsadmission.MutatingConfigName
	} else {
		pol.Status.WebhookConfigName = jsadmission.ValidatingConfigName
	}
	pol.Status.Instance = &corev1alpha1.JSInstanceStatus{
		StartedAt:          &startedAt,
		SourceHash:         srcHash,
		RestartCount:       mi.RestartCount,
		LastRestartReason:  mi.LastReason,
		ManualRestartToken: pol.GetAnnotations()[conditions.ManualRestartAnnotation],
	}
	if err := r.Status().Update(ctx, &pol); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *JSAdmissionReconciler) failAdmission(ctx context.Context, pol *corev1alpha1.JSAdmission, msg string) (ctrl.Result, error) {
	now := metav1.NewTime(time.Now())
	apimeta.SetStatusCondition(&pol.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionFalse,
		Reason:             conditions.ReasonFailed,
		Message:            msg,
		ObservedGeneration: pol.Generation,
	})
	pol.Status.ObservedGeneration = pol.Generation
	pol.Status.LastReview = &corev1alpha1.JSExecutionStatus{Time: &now, Error: msg}
	if err := r.Status().Update(ctx, pol); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// admissionLimitsFromSpec maps the CRD's optional Resources to jsengine.Limits.
func admissionLimitsFromSpec(r *corev1alpha1.JSResources) jsengine.Limits {
	if r == nil {
		return jsengine.Limits{}
	}
	return jsengine.Limits{MemoryMB: r.MemoryMB, TimeoutSeconds: r.TimeoutSeconds}
}

func orInt32(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}

// SetupWithManager sets up the controller with the Manager.
func (r *JSAdmissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Loader == nil {
		r.Loader = jssource.NewChain(jssource.InlineLoader{})
	}
	if r.Registry == nil {
		r.Registry = jsregistry.NewRegistry()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.JSAdmission{}).
		Named("jsadmission").
		Complete(r)
}
