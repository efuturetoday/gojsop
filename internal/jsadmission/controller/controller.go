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
	"errors"
	"fmt"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlsource "sigs.k8s.io/controller-runtime/pkg/source"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/conditions"
	"github.com/efuturetoday/gojsop/internal/jsaccess"
	"github.com/efuturetoday/gojsop/internal/jsadmission"
	"github.com/efuturetoday/gojsop/internal/jslifecycle"
	"github.com/efuturetoday/gojsop/internal/jsrun"
	"github.com/efuturetoday/gojsop/internal/jssource"
)

// JSAdmissionReconciler reports a JSAdmission in its status and publishes it
// to the two central WebhookConfigurations gojsop owns.
//
// It is leader-elected, because both of those have exactly one writer: N
// replicas writing the same status or the same WebhookConfiguration would
// fight over it. Preparing the script and answering requests is the job of
// JSAdmissionServerReconciler, which runs on every replica
// (jsadmission.R20).
type JSAdmissionReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Loader resolves spec.source to JS bytes (defaults to inline-only). It
	// is used only to report a loader failure; the script is prepared by the
	// server reconciler.
	Loader *jssource.Chain
	// Scripts is read, never driven: State tells what the runner holds for a
	// policy so the status describes what actually serves requests.
	Scripts jsrun.Scripts
	// Registrar maintains the central VWC/MWC.
	Registrar *jsadmission.Registrar

	// Access gives every policy a ServiceAccount of its own with the rights
	// it declares. Optional in tests.
	Access *jsaccess.Manager

	// Recorder publishes corev1.Event entries describing lifecycle moments
	// (build/restart/admission review crashes). Optional — nil-safe so unit
	// tests that build the reconciler bare keep working.
	Recorder events.EventRecorder
}

// eventAction is the action field every event carries; the events.k8s.io API
// requires one, and the reason already names the moment.
const eventAction = "Reconcile"

// event records a corev1.Event about obj. Nil-safe.
func (r *JSAdmissionReconciler) event(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, eventType, reason, eventAction, "%s", message)
	}
}

// +kubebuilder:rbac:groups=core.gojsop.io,resources=jsadmissions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jsadmissions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jsadmissions/finalizers,verbs=update
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations;mutatingwebhookconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// status-conditions.R1
// status-conditions.R2
// status-conditions.R5
func (r *JSAdmissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("jsadmission", req.Name)

	var pol corev1alpha1.JSAdmission
	if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("cleanup started", "phase", "delete")
			// The HTTP table and the prepared script belong to the server
			// controller, which runs on every replica and drops them there
			// (jsadmission.R20). The leader owns the central
			// WebhookConfigurations alone.
			if r.Registrar != nil {
				r.Registrar.Remove(req.NamespacedName)
			}
			log.V(1).Info("cleanup done", "phase", "delete")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	priorObservedGen := pol.Status.ObservedGeneration
	priorWebhookConfig := pol.Status.WebhookConfigName

	// The source is loaded here only to report a loader failure: the status
	// has exactly one writer, so the moment "your source cannot be read" has
	// to be seen by that writer. Preparing the script is the server
	// controller's job (jsadmission.R20).
	source, err := r.Loader.Load(ctx, pol.Spec.Source)
	if err != nil {
		log.Error(err, "loading admission source")
		return r.failAdmission(ctx, &pol, conditions.EventSourceLoadFailed,
			"source loader failed",
			fmt.Sprintf("source: %v", err))
	}
	srcHash := jssource.Hash(source)
	mutating := isMutating(&pol)

	// The policy's own ServiceAccount, which its kube.* calls run as on every
	// replica. Only the leader writes it.
	// kube-access.R10
	if r.Access != nil {
		sa, err := r.Access.Ensure(ctx, &pol, jsrun.KindJSAdmission, jsaccess.AdmissionRules(pol.Spec))
		if err != nil {
			log.Error(err, "setting up the policy's service account")
			return r.failAdmission(ctx, &pol, conditions.EventAccessFailed,
				"service account setup failed",
				fmt.Sprintf("service account: %v", err))
		}
		pol.Status.ServiceAccount = sa
	}

	resetToken := pol.GetAnnotations()[conditions.ManualRestartAnnotation]

	// What runs is what the runner holds, not what was just loaded. A key the
	// server controller has not reached yet, or one that still holds an older
	// source, counts as building — reporting Ready for a source no instance
	// serves would be a lie.
	// js-registry.R22
	// jsadmission.R18
	st, known := r.Scripts.State(jsrun.AdmissionKey(req.NamespacedName))
	if !known || st.SourceHash != srcHash {
		log.V(1).Info("instance not prepared for this source yet")
		return r.building(ctx, &pol)
	}
	switch st.Phase {
	case jsrun.PhasePreparing:
		log.V(1).Info("instance building")
		return r.building(ctx, &pol)
	case jsrun.PhaseFailed:
		log.Error(st.Err, "instance build failed", "attempts", st.Attempts, "retryIn", st.RetryIn())
		eventReason, eventMsg := conditions.ClassifyBuildError(st.Err)
		return r.buildFailed(ctx, &pol, eventReason, eventMsg, st)
	}
	last := st.Recoveries.Last()
	restarted := instanceChanged(&pol, srcHash)
	// A new value of the restart annotation is Spec.ResetToken: Ensure
	// prepared the script again with reason manual, in the background like any
	// other build. The status token tells the finished build is not seen twice.
	var prevToken string
	if pol.Status.Script != nil {
		prevToken = pol.Status.Script.RestartToken
	}
	manual := !restarted && resetToken != prevToken && last.Reason == jsrun.ReasonManual
	if restarted || manual {
		log.Info("instance (re)started", "hash", srcHash[:12], "restarts", len(st.Recoveries.Recent), "reason", last.Reason)
		if conditions.WasBuilding(pol.Status.Conditions) && last.Reason != "" {
			r.event(&pol, corev1.EventTypeNormal, conditions.EventRestarted,
				fmt.Sprintf("restarted: %s (hash %s)", last.Reason, srcHash[:12]))
		}
	}

	path := jsadmission.PathFor(req.NamespacedName, mutating)

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
			Key:           req.NamespacedName,
			Path:          path,
			Mutating:      mutating,
			Rules:         jsadmission.RulesFromAPI(apiRules),
			FailurePolicy: failurePolicyOf(&pol),
			MatchPolicy:   admissionregv1.MatchPolicyType(pol.Spec.MatchPolicy),
			// A policy only reads (jsadmission.R13), so it never has side effects.
			SideEffects:    admissionregv1.SideEffectClassNone,
			TimeoutSeconds: pol.Spec.TimeoutSeconds,
			NSSelector:     pol.Spec.NamespaceSelector,
			ObjectSelector: pol.Spec.ObjectSelector,
		})
	}

	// The registrar syncs in the background. Its last failure is reported
	// here, because the reconciler is the only writer of status; the
	// registrar retries and re-triggers this reconcile when it recovers.
	// jsadmission.R17
	if r.Registrar != nil {
		if syncErr := r.Registrar.SyncError(); syncErr != nil {
			log.Error(syncErr, "webhook configuration sync failing")
			return r.failAdmissionReason(ctx, &pol, conditions.ReasonWebhookSyncFailed,
				conditions.EventWebhookSyncFailed, "webhook sync failed",
				fmt.Sprintf("webhook sync: %v", syncErr))
		}
	}

	startedAt := metav1.NewTime(st.PreparedAt)
	apimeta.SetStatusCondition(&pol.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReconciled,
		Message:            "policy registered",
		ObservedGeneration: pol.Generation,
	})
	pol.Status.ObservedGeneration = pol.Generation
	pol.Status.LastReconcile = jslifecycle.ReconcileSucceeded(pol.Status.LastReconcile, time.Now())
	pol.Status.WebhookPath = path
	if mutating {
		pol.Status.WebhookConfigName = jsadmission.MutatingConfigName
	} else {
		pol.Status.WebhookConfigName = jsadmission.ValidatingConfigName
	}
	byReason, recent := jslifecycle.RestartHistoryFor(st.Recoveries)
	pol.Status.Script = &corev1alpha1.JSScriptStatus{
		PreparedAt:       &startedAt,
		SourceHash:       srcHash,
		RestartsByReason: byReason,
		RecentRestarts:   recent,
		RestartToken:     pol.GetAnnotations()[conditions.ManualRestartAnnotation],
	}
	if err := r.Status().Update(ctx, &pol); err != nil {
		return ctrl.Result{}, err
	}
	if priorWebhookConfig == "" && pol.Status.WebhookConfigName != "" {
		// First publish to the central VWC/MWC. Two stable values
		// (validating vs mutating config name), so the dedup window
		// collapses identical events safely.
		r.event(&pol, corev1.EventTypeNormal, conditions.EventWebhookRegistered,
			fmt.Sprintf("published to %s", pol.Status.WebhookConfigName))
	}
	if priorObservedGen != pol.Generation && !restarted {
		r.event(&pol, corev1.EventTypeNormal, conditions.EventReconciled,
			fmt.Sprintf("reconciled generation %d", pol.Generation))
	}
	return ctrl.Result{}, nil
}

// instanceChanged reports whether the status does not describe this instance
// yet: none was recorded, it describes another source, or the last reconcile
// failed after the build.
func instanceChanged(pol *corev1alpha1.JSAdmission, srcHash string) bool {
	inst := pol.Status.Script
	if inst == nil || inst.SourceHash != srcHash {
		return true
	}
	c := apimeta.FindStatusCondition(pol.Status.Conditions, conditions.Ready)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == conditions.ReasonFailed
}

// building writes Ready=False with the reason Building and returns without a
// requeue: the registry notifies when the build ends.
// jsadmission.R18
// status-conditions.R1
// status-conditions.R2
func (r *JSAdmissionReconciler) building(ctx context.Context, pol *corev1alpha1.JSAdmission) (ctrl.Result, error) {
	apimeta.SetStatusCondition(&pol.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionFalse,
		Reason:             conditions.ReasonBuilding,
		Message:            "JS instance is building",
		ObservedGeneration: pol.Generation,
	})
	pol.Status.ObservedGeneration = pol.Generation
	if err := r.Status().Update(ctx, pol); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// buildFailed records the failed build and requeues after the backoff the
// registry computed from the number of failed attempts.
// jsadmission.R18
// status-conditions.R1
// status-conditions.R2
func (r *JSAdmissionReconciler) buildFailed(ctx context.Context, pol *corev1alpha1.JSAdmission, eventReason, eventMsg string, st jsrun.State) (ctrl.Result, error) {
	return r.failAdmissionAfter(ctx, pol, conditions.ReasonBuildFailed, eventReason, eventMsg,
		fmt.Sprintf("instance: %v", st.Err), st.RetryIn())
}

// failAdmission emits a Warning event with the stable eventMsg template,
// then writes a Failed condition carrying the verbose conditionMsg. Mirrors
// JSHookReconciler.fail.
// status-conditions.R1
// status-conditions.R2
func (r *JSAdmissionReconciler) failAdmission(ctx context.Context, pol *corev1alpha1.JSAdmission, eventReason, eventMsg, conditionMsg string) (ctrl.Result, error) {
	return r.failAdmissionReason(ctx, pol, conditions.ReasonFailed, eventReason, eventMsg, conditionMsg)
}

// failAdmissionReason is failAdmission with an explicit condition Reason.
// status-conditions.R1
// status-conditions.R2
func (r *JSAdmissionReconciler) failAdmissionReason(ctx context.Context, pol *corev1alpha1.JSAdmission, condReason, eventReason, eventMsg, conditionMsg string) (ctrl.Result, error) {
	return r.failAdmissionAfter(ctx, pol, condReason, eventReason, eventMsg, conditionMsg, 5*time.Second)
}

// failAdmissionAfter is failAdmissionReason with an explicit requeue delay.
// status-conditions.R1
// status-conditions.R2
func (r *JSAdmissionReconciler) failAdmissionAfter(ctx context.Context, pol *corev1alpha1.JSAdmission, condReason, eventReason, eventMsg, conditionMsg string, requeue time.Duration) (ctrl.Result, error) {
	r.event(pol, corev1.EventTypeWarning, eventReason, eventMsg)
	now := metav1.NewTime(time.Now())
	apimeta.SetStatusCondition(&pol.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionFalse,
		Reason:             condReason,
		Message:            conditionMsg,
		ObservedGeneration: pol.Generation,
	})
	pol.Status.ObservedGeneration = pol.Generation
	pol.Status.LastReconcile = &corev1alpha1.JSReconcileStatus{Time: &now, Error: conditionMsg}
	if err := r.Status().Update(ctx, pol); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// SetupWithManager sets up the controller with the Manager.
//
// Watches ConfigMaps so spec.source.configMapRef edits trigger a reconcile;
// the mapper picks every JSAdmission whose ref matches the changed CM.
func (r *JSAdmissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Loader == nil {
		r.Loader = jssource.NewChain(jssource.InlineLoader{})
	}
	if r.Scripts == nil {
		return errors.New("jsrun.Scripts is required")
	}
	b := ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.JSAdmission{}).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(jssource.JSAdmissionConfigMapMapper(mgr.GetClient())),
		).
		Named("jsadmission")
	// One channel carries both triggers: finished builds of the registry and a
	// changed sync outcome of the registrar.
	ch := make(chan event.TypedGenericEvent[*corev1alpha1.JSAdmission], 64)
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		for key := range r.Scripts.Watch(ctx, jsrun.KindJSAdmission) {
			pol := &corev1alpha1.JSAdmission{ObjectMeta: metav1.ObjectMeta{Name: key.Name.Name, Namespace: key.Name.Namespace}}
			select {
			case ch <- event.TypedGenericEvent[*corev1alpha1.JSAdmission]{Object: pol}:
			case <-ctx.Done():
				return nil
			}
		}
		return nil
	})); err != nil {
		return err
	}
	b = b.WatchesRawSource(ctrlsource.Channel(ch, &handler.TypedEnqueueRequestForObject[*corev1alpha1.JSAdmission]{}))
	if r.Registrar != nil {
		// A change in the registrar's sync outcome re-reconciles every
		// published policy so Ready follows it.
		r.Registrar.OnSyncResult = func(error) {
			for _, k := range r.Registrar.Keys() {
				pol := &corev1alpha1.JSAdmission{ObjectMeta: metav1.ObjectMeta{Name: k.Name, Namespace: k.Namespace}}
				select {
				case ch <- event.TypedGenericEvent[*corev1alpha1.JSAdmission]{Object: pol}:
				default:
				}
			}
		}
	}
	return b.Complete(r)
}
