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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	"github.com/o-haase/gojsop/internal/jsengine"
	"github.com/o-haase/gojsop/internal/jshook"
	"github.com/o-haase/gojsop/internal/jshook/dispatcher"
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

	// Dispatcher subscribes hooks to Kubernetes events. Optional in tests.
	Dispatcher *dispatcher.Dispatcher

	// SubscribeCtx is the parent context handed to dispatcher.Subscribe so
	// that informer goroutines tear down when the manager stops.
	SubscribeCtx context.Context
}

// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks/finalizers,verbs=update
// MVP: hooks can watch and mutate any resource. Phase 2 will narrow this
// based on the bindings each hook actually declares (per-hook ServiceAccount).
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;create;update;patch;delete

// readConfig is the registry PostBuildHook that runs jshook.ReadConfig on a
// freshly built VM and stashes the *jshook.Config in ManagedVM.Extra. The
// reconciler reads it back via configFromExtra. This keeps the config off the
// engine and inside the feature package.
func readConfig(vm *jsengine.VM) (any, error) {
	cfg, err := jshook.ReadConfig(vm)
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

func (r *JSHookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("jshook", req.Name)

	var hook corev1alpha1.JSHook
	if err := r.Get(ctx, req.NamespacedName, &hook); err != nil {
		if apierrors.IsNotFound(err) {
			// Hook was deleted — tear down informers and close the instance.
			if r.Dispatcher != nil {
				r.Dispatcher.Drop(req.NamespacedName)
			}
			r.Registry.Drop(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	source, err := r.Loader.Load(ctx, hook.Spec.Source)
	if err != nil {
		log.Error(err, "loading hook source")
		return r.fail(ctx, &hook, fmt.Sprintf("source: %v", err))
	}
	srcHash := jssource.Hash(source)
	lim := limitsFromSpec(hook.Spec.Resources)

	mi, restarted, err := r.Registry.GetOrLoad(req.NamespacedName, source, srcHash, lim, readConfig)
	if err != nil {
		log.Error(err, "registry GetOrLoad")
		return r.fail(ctx, &hook, fmt.Sprintf("instance: %v", err))
	}
	if restarted {
		log.Info("instance (re)started", "hash", srcHash[:12], "restarts", mi.RestartCount, "reason", mi.LastReason)
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
			newMI, err := r.Registry.RestartByKey(req.NamespacedName, jsregistry.ReasonManual, readConfig)
			if err != nil {
				log.Error(err, "manual restart")
				return r.fail(ctx, &hook, fmt.Sprintf("manual restart: %v", err))
			}
			log.Info("manual restart applied", "token", token, "restarts", newMI.RestartCount)
			mi = newMI
			restarted = true
		}
	}

	cfg := configFromExtra(mi.Extra)
	if cfg == nil {
		return r.fail(ctx, &hook, "hook does not export a config() function")
	}

	if r.Dispatcher != nil && (restarted || hook.Status.ObservedGeneration != hook.Generation) {
		if err := r.Dispatcher.Subscribe(r.subscribeCtx(), req.NamespacedName, mi.VM, cfg); err != nil {
			log.Error(err, "subscribing bindings")
			return r.fail(ctx, &hook, fmt.Sprintf("subscribe: %v", err))
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
	hook.Status.Instance = &corev1alpha1.JSInstanceStatus{
		StartedAt:          &startedAt,
		SourceHash:         srcHash,
		RestartCount:       mi.RestartCount,
		LastRestartReason:  mi.LastReason,
		ManualRestartToken: hook.GetAnnotations()[ManualRestartAnnotation],
	}
	if err := r.Status().Update(ctx, &hook); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *JSHookReconciler) fail(ctx context.Context, hook *corev1alpha1.JSHook, msg string) (ctrl.Result, error) {
	now := metav1.NewTime(time.Now())
	apimeta.SetStatusCondition(&hook.Status.Conditions, metav1.Condition{
		Type:               conditions.Ready,
		Status:             metav1.ConditionFalse,
		Reason:             conditions.ReasonFailed,
		Message:            msg,
		ObservedGeneration: hook.Generation,
	})
	hook.Status.ObservedGeneration = hook.Generation
	hook.Status.LastExecution = &corev1alpha1.JSExecutionStatus{
		Time:  &now,
		Error: msg,
	}
	if err := r.Status().Update(ctx, hook); err != nil {
		return ctrl.Result{}, err
	}
	// shell-operator-style 5s backoff on failure.
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// limitsFromSpec maps the CRD's optional Resources to jsengine.Limits.
// Zero/missing fields fall back to engine defaults inside New().
func limitsFromSpec(r *corev1alpha1.JSResources) jsengine.Limits {
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
func (r *JSHookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Loader == nil {
		r.Loader = jssource.NewChain(jssource.InlineLoader{})
	}
	if r.Registry == nil {
		r.Registry = jsregistry.NewRegistry()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.JSHook{}).
		Named("jshook").
		Complete(r)
}
