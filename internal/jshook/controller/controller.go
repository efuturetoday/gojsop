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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jsengine/kubehost"
	"github.com/o-haase/gojsop/internal/jshook"
	"github.com/o-haase/gojsop/internal/jshook/dispatcher"
	"github.com/o-haase/gojsop/internal/jslifecycle"
	"github.com/o-haase/gojsop/internal/jsregistry"
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
	Registry *jsregistry.Registry

	// KubeHost mints the host-function surface installed on every JSHook VM.
	// SharedFactory hands out the same client to all hooks; Phase 2 swaps in
	// a per-ServiceAccount factory without touching this call site.
	KubeHost kubehost.Factory

	// Dispatcher subscribes hooks to Kubernetes events. Optional in tests.
	Dispatcher *dispatcher.Dispatcher

	// SubscribeCtx is the parent context handed to dispatcher.Subscribe so
	// that informer goroutines tear down when the manager stops.
	SubscribeCtx context.Context

	// Recorder publishes corev1.Event entries describing lifecycle moments
	// (build/restart/dispatcher rescue/handle errors). Optional — nil-safe
	// so unit tests that build the reconciler bare keep working. In
	// production cmd/main.go injects mgr.GetEventRecorderFor(...).
	Recorder record.EventRecorder
}

// event records a corev1.Event about obj. Nil-safe: a Reconciler built
// without a Recorder (the test default) silently no-ops.
func (r *JSHookReconciler) event(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, eventType, reason, message)
	}
}

// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// MVP: hooks can watch and mutate any resource. Phase 2 will narrow this
// based on the bindings each hook actually declares (per-hook ServiceAccount).
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;create;update;patch;delete

// readConfig is the registry PostBuildHook that runs jshook.ReadConfig on a
// freshly built VM and stashes the *jshook.Config in ManagedVM.Extra. The
// reconciler reads it back via configFromExtra. This keeps the config off the
// engine and inside the feature package.
// Block: hook-dispatch R1
func readConfig(ctx context.Context, vm *jsengine.VM) (any, error) {
	if !vm.HasExport("config") {
		return nil, &jsregistry.MissingExportError{Name: "config"}
	}
	if !vm.HasExport("handle") {
		return nil, &jsregistry.MissingExportError{Name: "handle"}
	}
	cfg, err := jshook.ReadConfig(ctx, vm)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func configFromExtra(extra any) *jshook.Config {
	if extra == nil {
		return nil
	}
	if cfg, ok := extra.(*jshook.Config); ok {
		return cfg
	}
	return nil
}

// Block: hook-dispatch R8
// Block: status-conditions R1
// Block: status-conditions R2
// Block: status-conditions R5
func (r *JSHookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("jshook", req.Name)

	var hook corev1alpha1.JSHook
	if err := r.Get(ctx, req.NamespacedName, &hook); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("cleanup started", "phase", "delete")
			if r.Dispatcher != nil {
				r.Dispatcher.Drop(req.NamespacedName)
			}
			r.Registry.Drop(req.NamespacedName)
			log.Info("cleanup done", "phase", "delete")
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

	var binder jsengine.HostBinder
	if r.KubeHost != nil {
		binder, err = r.KubeHost.ForHook(ctx, req.NamespacedName, "")
		if err != nil {
			log.Error(err, "minting kube host binder")
			return r.fail(ctx, &hook, conditions.EventBuildFailed,
				"build failed: kube host",
				fmt.Sprintf("kube host: %v", err))
		}
	}

	mi, restarted, err := r.Registry.GetOrLoad(ctx, req.NamespacedName, jsregistry.BuildOptions{
		Source:     source,
		SourceHash: srcHash,
		Limits:     lim,
		Binder:     binder,
		PostBuild:  readConfig,
	})
	if err != nil {
		log.Error(err, "registry GetOrLoad")
		eventReason, eventMsg := conditions.ClassifyBuildError(err)
		return r.fail(ctx, &hook, eventReason, eventMsg, fmt.Sprintf("instance: %v", err))
	}
	if restarted {
		last := mi.LastRestart()
		log.Info("instance (re)started", "hash", srcHash[:12], "restarts", len(mi.History), "reason", last.Reason)
		r.event(&hook, corev1.EventTypeNormal, conditions.EventRestarted,
			fmt.Sprintf("restarted: %s (hash %s)", last.Reason, srcHash[:12]))
	}

	// Manual-restart annotation: a new value of gojsop.io/restart triggers
	// exactly one rebuild. We compare against status.instance.manualRestartToken
	// so the contract is "edit the annotation to force a restart" without
	// needing controller-internal flags.
	if token, ok := hook.GetAnnotations()[ManualRestartAnnotation]; ok && !restarted {
		var prev string
		if hook.Status.Instance != nil {
			prev = hook.Status.Instance.ManualRestartToken
		}
		if token != prev {
			newMI, err := r.Registry.RestartByKey(req.NamespacedName, jsregistry.ReasonManual)
			if err != nil {
				log.Error(err, "manual restart")
				return r.fail(ctx, &hook, conditions.EventBuildFailed,
					"build failed: manual restart",
					fmt.Sprintf("manual restart: %v", err))
			}
			last := newMI.LastRestart()
			log.Info("manual restart applied", "token", token, "restarts", len(newMI.History))
			r.event(&hook, corev1.EventTypeNormal, conditions.EventRestarted,
				fmt.Sprintf("restarted: %s (hash %s)", last.Reason, srcHash[:12]))
			mi = newMI
			restarted = true
		}
	}

	cfg := configFromExtra(mi.Extra)
	if cfg == nil {
		return r.fail(ctx, &hook, conditions.EventConfigInvalid,
			"config() returned non-object",
			"hook does not export a config() function")
	}

	if r.Dispatcher != nil && (restarted || hook.Status.ObservedGeneration != hook.Generation) {
		// Bind a copy of the hook into the emitter closure so the recorder
		// has a target with stable UID/ObjectMeta for the lifetime of the
		// subscription. The closure is replaced on every (re)Subscribe.
		hookForEvents := hook.DeepCopy()
		emit := func(eventType, reason, message string) {
			r.event(hookForEvents, eventType, reason, message)
		}
		if err := r.Dispatcher.Subscribe(r.subscribeCtx(), req.NamespacedName, cfg, emit); err != nil {
			log.Error(err, "subscribing bindings")
			return r.fail(ctx, &hook, conditions.EventSubscribeFailed,
				"subscribe failed",
				fmt.Sprintf("subscribe: %v", err))
		}
	}

	bindings := summarizeBindings(cfg)
	startedAt := metav1.NewTime(mi.StartedAt)
	apimeta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReconciled,
		Message:            "JS instance running",
		ObservedGeneration: hook.Generation,
	})
	hook.Status.ObservedGeneration = hook.Generation
	hook.Status.Bindings = bindings
	byReason, recent := jslifecycle.RestartHistoryFor(mi)
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

// fail records a Warning Event and writes a Failed condition. eventReason is
// one of the EventXxxFailed reasons in the conditions package; eventMsg is
// the static, low-cardinality template (see plan's "Message stability"
// section). conditionMsg may carry the verbose error string — that is
// per-CR and not subject to the recorder's dedup window.
// Block: status-conditions R1
// Block: status-conditions R2
func (r *JSHookReconciler) fail(ctx context.Context, hook *corev1alpha1.JSHook, eventReason, eventMsg, conditionMsg string) (ctrl.Result, error) {
	r.event(hook, corev1.EventTypeWarning, eventReason, eventMsg)
	now := metav1.NewTime(time.Now())
	apimeta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionFalse,
		Reason:             conditions.ReasonFailed,
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
	// shell-operator-style 5s backoff on failure.
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// limitsFromSpec maps the CRD's optional Limits to jsengine.Limits.
// Zero/missing fields fall back to engine defaults inside New().
func limitsFromSpec(r *corev1alpha1.JSLimits) jsengine.Limits {
	if r == nil {
		return jsengine.Limits{}
	}
	return jsengine.Limits{
		MemoryMB:       r.MemoryMB,
		TimeoutSeconds: r.TimeoutSeconds,
	}
}

func summarizeBindings(cfg *jshook.Config) []string {
	out := make([]string, 0, len(cfg.Kubernetes)+len(cfg.Schedule))
	for _, b := range cfg.Kubernetes {
		out = append(out, fmt.Sprintf("kubernetes:%s/%s/%s", b.APIVersion, b.Kind, b.Name))
	}
	for _, s := range cfg.Schedule {
		out = append(out, fmt.Sprintf("schedule:%s/%s", s.Crontab, s.Name))
	}
	if cfg.OnStartup > 0 {
		out = append(out, fmt.Sprintf("onStartup:%d", cfg.OnStartup))
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
	if r.Registry == nil {
		r.Registry = jsregistry.NewRegistry()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.JSHook{}).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(jssource.JSHookConfigMapMapper(mgr.GetClient())),
		).
		Named("jshook").
		Complete(r)
}
