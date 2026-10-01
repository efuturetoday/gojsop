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

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsadmission"
	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsengine/kubehost"
	"github.com/o-haase/gojsop/internal/jslifecycle"
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
	// KubeHost mints the host-function surface installed on every JSAdmission VM.
	// ForAdmission returns a read-only binder — admission policies must not
	// write to the cluster from the apiserver request path (sideEffects:
	// None contract), so apply/delete are intentionally not bound.
	KubeHost kubehost.Factory
	// Server holds the live policy table the HTTP webhook handler consults.
	Server *jsadmission.Server
	// Registrar maintains the central VWC/MWC.
	Registrar *jsadmission.Registrar

	// Recorder publishes corev1.Event entries describing lifecycle moments
	// (build/restart/admission review crashes). Optional — nil-safe so unit
	// tests that build the reconciler bare keep working.
	Recorder events.EventRecorder

	// Backoff spaces the retries of a policy whose build failed. Zero fields
	// use the jsregistry defaults. cmd/main.go sets it from the operator flags.
	Backoff jsregistry.Backoff
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

// admissionPostBuild returns a PostBuild closure that asserts the loaded
// module exposes the entrypoint required by the policy's spec.type
// (validate for validating, mutate for mutating). A missing entrypoint is
// surfaced as a typed MissingExportError so the reconciler can map it to
// the EntrypointMissing event reason without sniffing message strings.
func admissionPostBuild(mutating bool) jsregistry.PostBuildHook {
	entry := "validate"
	if mutating {
		entry = "mutate"
	}
	return func(ctx context.Context, vm *jsengine.VM) (any, error) {
		_ = ctx
		if !vm.HasExport(entry) {
			return nil, &jsregistry.MissingExportError{Name: entry}
		}
		return nil, nil
	}
}

// status-conditions.R1
// status-conditions.R2
// status-conditions.R5
func (r *JSAdmissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("jsadmission", req.Name)

	var pol corev1alpha1.JSAdmission
	if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("cleanup started", "phase", "delete")
			if r.Server != nil {
				r.Server.Unregister(req.NamespacedName)
			}
			if r.Registrar != nil {
				r.Registrar.Remove(req.NamespacedName)
			}
			r.Registry.Drop(jsregistry.AdmissionKey(req.NamespacedName))
			log.Info("cleanup done", "phase", "delete")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	priorObservedGen := pol.Status.ObservedGeneration
	priorWebhookConfig := pol.Status.WebhookConfigName

	source, err := r.Loader.Load(ctx, pol.Spec.Source)
	if err != nil {
		log.Error(err, "loading admission source")
		return r.failAdmission(ctx, &pol, conditions.EventSourceLoadFailed,
			"source loader failed",
			fmt.Sprintf("source: %v", err))
	}
	srcHash := jssource.Hash(source)
	lim := admissionLimitsFromSpec(pol.Spec.Limits)
	mutating := pol.Spec.Type == "mutating"

	var binder jsengine.HostBinder
	if r.KubeHost != nil {
		binder, err = r.KubeHost.ForAdmission(ctx, req.NamespacedName, "")
		if err != nil {
			log.Error(err, "minting kube host binder")
			return r.failAdmission(ctx, &pol, conditions.EventBuildFailed,
				"build failed: kube host",
				fmt.Sprintf("kube host: %v", err))
		}
	}

	// The build runs in the background; the registry notifies this controller
	// when it ends (SetupWithManager), so a reconcile never waits for it.
	// js-registry.R15
	// jsadmission.R18
	st := r.Registry.Ensure(jsregistry.AdmissionKey(req.NamespacedName), jsregistry.BuildOptions{
		Source:     source,
		SourceHash: srcHash,
		Limits:     lim,
		Binder:     binder,
		PostBuild:  admissionPostBuild(mutating),
		Backoff:    r.Backoff,
	})
	switch st.Kind {
	case jsregistry.StateBuilding:
		log.V(1).Info("instance building")
		return r.building(ctx, &pol)
	case jsregistry.StateBroken:
		log.Error(st.Err, "instance build failed", "attempts", st.Attempts, "retryIn", st.RetryIn())
		eventReason, eventMsg := conditions.ClassifyBuildError(st.Err)
		return r.buildFailed(ctx, &pol, eventReason, eventMsg, st)
	}
	mi := st.VM
	restarted := instanceChanged(&pol, srcHash)
	if restarted {
		last := mi.LastRestart()
		log.Info("instance (re)started", "hash", srcHash[:12], "restarts", len(mi.History), "reason", last.Reason)
		if conditions.WasBuilding(pol.Status.Conditions) && last.Reason.ReportedByReconcile() {
			r.event(&pol, corev1.EventTypeNormal, conditions.EventRestarted,
				fmt.Sprintf("restarted: %s (hash %s)", last.Reason, srcHash[:12]))
		}
	}

	// Manual-restart annotation, mirrors the JSHook flow.
	if token, ok := pol.GetAnnotations()[conditions.ManualRestartAnnotation]; ok && !restarted {
		var prev string
		if pol.Status.Instance != nil {
			prev = pol.Status.Instance.ManualRestartToken
		}
		if token != prev {
			// The restart builds in the background like any other build; the
			// token is recorded now so the finished build does not restart
			// again.
			if err := r.Registry.RestartByKey(jsregistry.AdmissionKey(req.NamespacedName), jsregistry.ReasonManual); err != nil {
				log.Error(err, "manual restart")
				return r.failAdmission(ctx, &pol, conditions.EventBuildFailed,
					"build failed: manual restart",
					fmt.Sprintf("manual restart: %v", err))
			}
			log.Info("manual restart started", "token", token)
			r.event(&pol, corev1.EventTypeNormal, conditions.EventRestarted,
				fmt.Sprintf("restarted: %s (hash %s)", jsregistry.ReasonManual, srcHash[:12]))
			if pol.Status.Instance == nil {
				pol.Status.Instance = &corev1alpha1.JSInstanceStatus{}
			}
			pol.Status.Instance.ManualRestartToken = token
			return r.building(ctx, &pol)
		}
	}

	path := jsadmission.PathFor(req.NamespacedName, mutating)
	timeout := time.Duration(orInt32(pol.Spec.TimeoutSeconds, 5)) * time.Second

	// Publish to the HTTP server first — once the central VWC/MWC points at us,
	// requests start arriving and a missing entry would 404 under FailurePolicy.
	if r.Server != nil {
		// Snapshot the policy meta into the closure so the recorder has a
		// stable target for the lifetime of this server registration. The
		// closure is replaced on every reconcile that re-Registers.
		polForEvents := pol.DeepCopy()
		emit := func(eventType, reason, message string) {
			r.event(polForEvents, eventType, reason, message)
		}
		r.Server.Register(jsadmission.PolicyEntry{
			Key:           req.NamespacedName,
			Mutating:      mutating,
			Timeout:       timeout,
			FailurePolicy: admissionregv1.FailurePolicyType(pol.Spec.FailurePolicy),
			Emit:          emit,
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
	byReason, recent := jslifecycle.RestartHistoryFor(mi)
	pol.Status.Instance = &corev1alpha1.JSInstanceStatus{
		StartedAt:          &startedAt,
		SourceHash:         srcHash,
		RestartsByReason:   byReason,
		RecentRestarts:     recent,
		ManualRestartToken: pol.GetAnnotations()[conditions.ManualRestartAnnotation],
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
	inst := pol.Status.Instance
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
func (r *JSAdmissionReconciler) buildFailed(ctx context.Context, pol *corev1alpha1.JSAdmission, eventReason, eventMsg string, st jsregistry.State) (ctrl.Result, error) {
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

// admissionLimitsFromSpec maps the CRD's optional Limits to jsengine.Limits.
func admissionLimitsFromSpec(r *corev1alpha1.JSLimits) jsengine.Limits {
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
//
// Watches ConfigMaps so spec.source.configMapRef edits trigger a reconcile;
// the mapper picks every JSAdmission whose ref matches the changed CM.
func (r *JSAdmissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Loader == nil {
		r.Loader = jssource.NewChain(jssource.InlineLoader{})
	}
	if r.Registry == nil {
		r.Registry = jsregistry.NewRegistry()
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
		for key := range r.Registry.Watch(ctx, jsregistry.KindJSAdmission) {
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
