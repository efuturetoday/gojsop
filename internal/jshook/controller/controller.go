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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
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
	"github.com/o-haase/gojsop/internal/jsaccess"
	"github.com/o-haase/gojsop/internal/jsengine/kubehost"
	"github.com/o-haase/gojsop/internal/jshook/dispatcher"
	"github.com/o-haase/gojsop/internal/jslifecycle"
	"github.com/o-haase/gojsop/internal/jsrun"
	"github.com/o-haase/gojsop/internal/jssource"
)

// ManualRestartAnnotation is the JSHook annotation that triggers a manual
// instance restart. Aliased here for callers that already imported it from
// this package; the source of truth is conditions.ManualRestartAnnotation.
const ManualRestartAnnotation = conditions.ManualRestartAnnotation

// JSHookReconciler reconciles a JSHook object.
//
// Each JSHook is backed by exactly one persistent QuickJS instance held in
// Registry. The instance survives across reconciles so that JS module-top-level
// state (globalThis caches, counters, expensive setups) persists. A change in
// spec.source's sha256 triggers a controlled restart; deletion drops it.
type JSHookReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Loader is the chain that resolves spec.source to JS bytes.
	// Defaults to inline-only via SetupWithManager when nil.
	Loader *jssource.Chain

	// Registry owns the per-hook persistent JS instances.
	// Defaults to a fresh registry via SetupWithManager when nil.
	Scripts jsrun.Scripts

	// KubeHost mints the host-function surface installed on every JSHook VM.
	// SharedFactory hands out the same client to all hooks; Phase 2 swaps in
	// a per-ServiceAccount factory without touching this call site.
	// SetupWithManager requires it; a reconciler built bare in a unit test
	// gives scripts no kube global.
	KubeHost kubehost.Factory

	// Access gives every hook a ServiceAccount of its own with the rights it
	// declares; its watches and kube.* calls run as that ServiceAccount.
	// Optional in tests: without it the hook uses the operator's client.
	Access *jsaccess.Manager

	// Dispatcher subscribes hooks to Kubernetes events. Optional in tests.
	Dispatcher *dispatcher.Dispatcher

	// SubscribeCtx is the parent context handed to dispatcher.Subscribe so
	// that informer goroutines tear down when the manager stops.
	SubscribeCtx context.Context

	// Recorder publishes corev1.Event entries describing lifecycle moments
	// (build/restart/handle errors). Optional — nil-safe
	// so unit tests that build the reconciler bare keep working. In
	// production cmd/main.go injects mgr.GetEventRecorder(...).
	Recorder events.EventRecorder

	// Backoff spaces the retries of a hook whose build failed. Zero fields use
	// the jsrun defaults. cmd/main.go sets it from the operator flags.
	Backoff jsrun.Backoff
}

// eventAction is the action field every event carries; the events.k8s.io API
// requires one, and the reason already names the moment.
const eventAction = "Reconcile"

// event records a corev1.Event about obj. Nil-safe: a Reconciler built
// without a Recorder (the test default) silently no-ops.
func (r *JSHookReconciler) event(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, eventType, reason, eventAction, "%s", message)
	}
}

// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks/finalizers,verbs=update

// The dispatcher resolves a binding's namespaceSelector against namespace
// labels, so the operator itself has to read namespaces (jshook.R22).
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// requireHandle is the registry PostBuildHook. A hook's script has to export
// handle(); what it reacts to is spec.bindings, not something the script
// returns, so nothing else has to run at build time.
// jshook.R2
func requireHandle(_ context.Context, s jsrun.Script) error {
	if !s.HasExport("handle") {
		return &jsrun.MissingExportError{Name: "handle"}
	}
	return nil
}

// jshook.R14
// status-conditions.R1
// status-conditions.R2
// status-conditions.R5
func (r *JSHookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("jshook", req.Name)

	var hook corev1alpha1.JSHook
	if err := r.Get(ctx, req.NamespacedName, &hook); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(1).Info("cleanup started", "phase", "delete")
			if r.Dispatcher != nil {
				r.Dispatcher.Drop(req.NamespacedName)
			}
			r.Scripts.Drop(jsrun.HookKey(req.NamespacedName))
			log.V(1).Info("cleanup done", "phase", "delete")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	priorObservedGen := hook.Status.ObservedGeneration

	source, err := r.Loader.Load(ctx, hook.Spec.Source)
	if err != nil {
		log.Error(err, "loading hook source")
		return r.fail(ctx, &hook, conditions.EventSourceLoadFailed,
			"source loader failed",
			fmt.Sprintf("source: %v", err))
	}
	srcHash := jssource.Hash(source)
	lim := limitsFromSpec(hook.Spec.Limits)

	// The hook's own ServiceAccount, with exactly its rights, exists before
	// anything runs as it.
	// kube-access.R10
	var sa string
	var as dynamic.Interface
	if r.Access != nil {
		sa, err = r.Access.Ensure(ctx, &hook, jsrun.KindJSHook, jsaccess.HookRules(hook.Spec))
		if err == nil {
			as, err = r.Access.ClientFor(sa)
		}
		if err != nil {
			log.Error(err, "setting up the hook's service account")
			return r.fail(ctx, &hook, conditions.EventAccessFailed,
				"service account setup failed",
				fmt.Sprintf("service account: %v", err))
		}
		hook.Status.ServiceAccount = sa
	}

	var host jsrun.Host
	if r.KubeHost != nil {
		host, err = r.KubeHost.ForHook(ctx, req.NamespacedName, sa)
		if err != nil {
			log.Error(err, "minting kube host")
			return r.fail(ctx, &hook, conditions.EventBuildFailed,
				"build failed: kube host",
				fmt.Sprintf("kube host: %v", err))
		}
	}

	resetToken := hook.GetAnnotations()[ManualRestartAnnotation]

	// The build runs in the background; the registry notifies this controller
	// when it ends (SetupWithManager), so a reconcile never waits for it.
	// js-registry.R15
	// jshook.R18
	st := r.Scripts.Ensure(jsrun.HookKey(req.NamespacedName), jsrun.Spec{
		Source:     source,
		SourceHash: srcHash,
		Limits:     lim,
		Host:       host,
		PostBuild:  requireHandle,
		ResetToken: resetToken,
		Backoff:    r.Backoff,
	})
	switch st.Phase {
	case jsrun.PhasePreparing:
		log.V(1).Info("instance building")
		return r.building(ctx, &hook)
	case jsrun.PhaseFailed:
		log.Error(st.Err, "instance build failed", "attempts", st.Attempts, "retryIn", st.RetryIn())
		eventReason, eventMsg := conditions.ClassifyBuildError(st.Err)
		return r.buildFailed(ctx, &hook, eventReason, eventMsg, st)
	}
	last := st.Recoveries.Last()
	restarted := instanceChanged(&hook, srcHash)
	// A new value of the restart annotation is Spec.ResetToken: Ensure
	// prepared the script again with reason manual, in the background like any
	// other build. The status token tells the finished build is not seen twice.
	var prevToken string
	if hook.Status.Instance != nil {
		prevToken = hook.Status.Instance.ManualRestartToken
	}
	manual := !restarted && resetToken != prevToken && last.Reason == jsrun.ReasonManual
	if restarted || manual {
		log.Info("instance (re)started", "hash", srcHash[:12], "restarts", len(st.Recoveries.Recent), "reason", last.Reason)
		if conditions.WasBuilding(hook.Status.Conditions) && last.Reason != "" {
			r.event(&hook, corev1.EventTypeNormal, conditions.EventRestarted,
				fmt.Sprintf("restarted: %s (hash %s)", last.Reason, srcHash[:12]))
		}
	}

	if r.Dispatcher != nil && (restarted || hook.Status.ObservedGeneration != hook.Generation) {
		// Bind a copy of the hook into the emitter closure so the recorder
		// has a target with stable UID/ObjectMeta for the lifetime of the
		// subscription. The closure is replaced on every (re)Subscribe.
		hookForEvents := hook.DeepCopy()
		emit := func(eventType, reason, message string) {
			r.event(hookForEvents, eventType, reason, message)
		}
		if err := r.Dispatcher.Subscribe(r.subscribeCtx(), req.NamespacedName, hook.Spec.Bindings, as, emit); err != nil {
			log.Error(err, "subscribing bindings")
			return r.fail(ctx, &hook, conditions.EventSubscribeFailed,
				"subscribe failed",
				fmt.Sprintf("subscribe: %v", err))
		}
	}

	bindings := summarizeBindings(hook.Spec.Bindings)
	startedAt := metav1.NewTime(st.PreparedAt)
	apimeta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReconciled,
		Message:            "JS instance running",
		ObservedGeneration: hook.Generation,
	})
	hook.Status.ObservedGeneration = hook.Generation
	hook.Status.Bindings = bindings
	hook.Status.LastReconcile = jslifecycle.ReconcileSucceeded(hook.Status.LastReconcile, time.Now())
	byReason, recent := jslifecycle.RestartHistoryFor(st.Recoveries)
	hook.Status.Instance = &corev1alpha1.JSInstanceStatus{
		StartedAt:          &startedAt,
		SourceHash:         srcHash,
		RestartsByReason:   byReason,
		RecentRestarts:     recent,
		ManualRestartToken: hook.GetAnnotations()[ManualRestartAnnotation],
	}
	if err := r.Status().Update(ctx, &hook); err != nil {
		return ctrl.Result{}, err
	}
	if priorObservedGen != hook.Generation && !restarted {
		// One Reconciled event per spec edit. A restart already implies a
		// transition (and emitted its own Restarted event), so suppress the
		// duplicate here.
		r.event(&hook, corev1.EventTypeNormal, conditions.EventReconciled,
			fmt.Sprintf("reconciled generation %d", hook.Generation))
	}
	return ctrl.Result{}, nil
}

// instanceChanged reports whether the bindings must be (re)subscribed: the
// status does not describe an instance yet, it describes another source, or the
// last reconcile failed after the build. A rebuild of the same source (limits
// changed, manual restart) keeps the subscription.
func instanceChanged(hook *corev1alpha1.JSHook, srcHash string) bool {
	inst := hook.Status.Instance
	if inst == nil || inst.SourceHash != srcHash {
		return true
	}
	c := apimeta.FindStatusCondition(hook.Status.Conditions, conditions.Ready)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == conditions.ReasonFailed
}

// building writes Ready=False with the reason Building and returns without a
// requeue: the registry notifies when the build ends.
// jshook.R18
// status-conditions.R1
// status-conditions.R2
func (r *JSHookReconciler) building(ctx context.Context, hook *corev1alpha1.JSHook) (ctrl.Result, error) {
	apimeta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionFalse,
		Reason:             conditions.ReasonBuilding,
		Message:            "JS instance is building",
		ObservedGeneration: hook.Generation,
	})
	hook.Status.ObservedGeneration = hook.Generation
	if err := r.Status().Update(ctx, hook); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// buildFailed records the failed build and requeues after the backoff the
// registry computed from the number of failed attempts.
// jshook.R18
// status-conditions.R1
// status-conditions.R2
func (r *JSHookReconciler) buildFailed(ctx context.Context, hook *corev1alpha1.JSHook, eventReason, eventMsg string, st jsrun.State) (ctrl.Result, error) {
	return r.failReason(ctx, hook, conditions.ReasonBuildFailed, eventReason, eventMsg,
		fmt.Sprintf("instance: %v", st.Err), st.RetryIn())
}

// fail records a Warning Event and writes a Failed condition. eventReason is
// one of the EventXxxFailed reasons in the conditions package; eventMsg is
// the static, low-cardinality template (see plan's "Message stability"
// section). conditionMsg may carry the verbose error string — that is
// per-CR and not subject to the recorder's dedup window.
// status-conditions.R1
// status-conditions.R2
func (r *JSHookReconciler) fail(ctx context.Context, hook *corev1alpha1.JSHook, eventReason, eventMsg, conditionMsg string) (ctrl.Result, error) {
	// shell-operator-style 5s backoff on failure.
	return r.failReason(ctx, hook, conditions.ReasonFailed, eventReason, eventMsg, conditionMsg, 5*time.Second)
}

// failReason is fail with an explicit condition Reason and requeue delay.
// status-conditions.R1
// status-conditions.R2
func (r *JSHookReconciler) failReason(ctx context.Context, hook *corev1alpha1.JSHook, condReason, eventReason, eventMsg, conditionMsg string, requeue time.Duration) (ctrl.Result, error) {
	r.event(hook, corev1.EventTypeWarning, eventReason, eventMsg)
	now := metav1.NewTime(time.Now())
	apimeta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionFalse,
		Reason:             condReason,
		Message:            conditionMsg,
		ObservedGeneration: hook.Generation,
	})
	hook.Status.ObservedGeneration = hook.Generation
	hook.Status.LastReconcile = &corev1alpha1.JSReconcileStatus{
		Time:  &now,
		Error: conditionMsg,
	}
	if err := r.Status().Update(ctx, hook); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// limitsFromSpec maps the CRD's optional Limits to jsrun.Limits.
// Zero/missing fields fall back to engine defaults inside New().
func limitsFromSpec(r *corev1alpha1.JSLimits) jsrun.Limits {
	if r == nil {
		return jsrun.Limits{}
	}
	return jsrun.Limits{
		MemoryMB:       r.MemoryMB,
		TimeoutSeconds: r.TimeoutSeconds,
	}
}

// summarizeBindings renders status.bindings: one line per binding and
// resource, in the form "name:group/version/resource". Every line is a watch
// the operator actually established — there is no inactive entry, because a
// binding the dispatcher cannot start fails the reconcile instead.
// jshook.R23
func summarizeBindings(bindings []corev1alpha1.HookBinding) []string {
	out := make([]string, 0, len(bindings))
	for _, b := range bindings {
		for _, group := range b.APIGroups {
			for _, version := range b.APIVersions {
				gv := schema.GroupVersion{Group: group, Version: version}.String()
				for _, resource := range b.Resources {
					out = append(out, fmt.Sprintf("%s:%s/%s", b.Name, gv, resource))
				}
			}
		}
	}
	return out
}

func (r *JSHookReconciler) subscribeCtx() context.Context {
	if r.SubscribeCtx != nil {
		return r.SubscribeCtx
	}
	return context.Background()
}

// SetupWithManager sets up the controller with the Manager.
//
// Watches ConfigMaps too: when a ConfigMap changes the mapper finds every
// JSHook whose spec.source.configMapRef points at it and enqueues a
// reconcile. That's how source-from-ConfigMap stays live without polling.
func (r *JSHookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Loader == nil {
		r.Loader = jssource.NewChain(jssource.InlineLoader{})
	}
	if r.Scripts == nil {
		return errors.New("jsrun.Scripts is required")
	}
	// kube-access.R8
	if r.KubeHost == nil {
		return errors.New("kubehost.Factory is required: without it hooks get no kube global")
	}
	// The registry reports every finished build of a JSHook; the forwarder
	// turns it into a reconcile request.
	ch := make(chan event.TypedGenericEvent[*corev1alpha1.JSHook])
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		for key := range r.Scripts.Watch(ctx, jsrun.KindJSHook) {
			hook := &corev1alpha1.JSHook{ObjectMeta: metav1.ObjectMeta{Name: key.Name.Name, Namespace: key.Name.Namespace}}
			select {
			case ch <- event.TypedGenericEvent[*corev1alpha1.JSHook]{Object: hook}:
			case <-ctx.Done():
				return nil
			}
		}
		return nil
	})); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.JSHook{}).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(jssource.JSHookConfigMapMapper(mgr.GetClient())),
		).
		WatchesRawSource(ctrlsource.Channel(ch, &handler.TypedEnqueueRequestForObject[*corev1alpha1.JSHook]{})).
		Named("jshook").
		Complete(r)
}
