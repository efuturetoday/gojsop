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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/dispatcher"
	"github.com/o-haase/gojsop/internal/hooks"
	jsruntime "github.com/o-haase/gojsop/internal/runtime"
)

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
	Loader *hooks.Chain

	// Registry owns the per-hook persistent JS instances.
	// Defaults to a fresh registry via SetupWithManager when nil.
	Registry *jsruntime.Registry

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
	srcHash := hooks.Hash(source)

	mi, restarted, err := r.Registry.GetOrLoad(req.NamespacedName, source, srcHash)
	if err != nil {
		log.Error(err, "registry GetOrLoad")
		return r.fail(ctx, &hook, fmt.Sprintf("instance: %v", err))
	}
	if restarted {
		log.Info("instance (re)started", "hash", srcHash[:12], "restarts", mi.RestartCount, "reason", mi.LastReason)
	}

	cfg, err := mi.Instance.LoadConfig()
	if err != nil {
		log.Error(err, "calling config()")
		return r.fail(ctx, &hook, fmt.Sprintf("config(): %v", err))
	}

	if r.Dispatcher != nil && (restarted || hook.Status.ObservedGeneration != hook.Generation) {
		if err := r.Dispatcher.Subscribe(r.subscribeCtx(), req.NamespacedName, mi.Instance, cfg); err != nil {
			log.Error(err, "subscribing bindings")
			return r.fail(ctx, &hook, fmt.Sprintf("subscribe: %v", err))
		}
	}

	bindings := summarizeBindings(cfg)
	startedAt := metav1.NewTime(mi.StartedAt)
	hook.Status.Phase = "Ready"
	hook.Status.ObservedGeneration = hook.Generation
	hook.Status.Bindings = bindings
	hook.Status.Instance = &corev1alpha1.JSHookInstanceStatus{
		StartedAt:         &startedAt,
		SourceHash:        srcHash,
		RestartCount:      mi.RestartCount,
		LastRestartReason: mi.LastReason,
	}
	if err := r.Status().Update(ctx, &hook); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *JSHookReconciler) fail(ctx context.Context, hook *corev1alpha1.JSHook, msg string) (ctrl.Result, error) {
	now := metav1.NewTime(time.Now())
	hook.Status.Phase = "Failed"
	hook.Status.ObservedGeneration = hook.Generation
	hook.Status.LastExecution = &corev1alpha1.JSHookExecutionStatus{
		Time:  &now,
		Error: msg,
	}
	if err := r.Status().Update(ctx, hook); err != nil {
		return ctrl.Result{}, err
	}
	// shell-operator-style 5s backoff on failure.
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func summarizeBindings(cfg *jsruntime.Config) []string {
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
		r.Loader = hooks.NewChain(hooks.InlineLoader{})
	}
	if r.Registry == nil {
		r.Registry = jsruntime.NewRegistry()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.JSHook{}).
		Named("jshook").
		Complete(r)
}
