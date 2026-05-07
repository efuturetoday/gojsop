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
	"github.com/o-haase/gojsop/internal/hooks"
	jsruntime "github.com/o-haase/gojsop/internal/runtime"
)

// JSHookReconciler reconciles a JSHook object.
//
// MVP behavior: on every reconcile we re-load the source, instantiate a fresh
// QuickJS runtime, call config(), and write the resolved bindings back into
// status. The persistent-instance registry replaces the per-reconcile
// instantiation in the next iteration (task: persistent registry).
type JSHookReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Loader is the chain that resolves spec.source to JS bytes.
	// Defaults to inline-only via SetupWithManager when nil.
	Loader *hooks.Chain
}

// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.gojsop.io,resources=jshooks/finalizers,verbs=update

func (r *JSHookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("jshook", req.Name)

	var hook corev1alpha1.JSHook
	if err := r.Get(ctx, req.NamespacedName, &hook); err != nil {
		if apierrors.IsNotFound(err) {
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
	log.Info("loaded hook source", "bytes", len(source), "hash", srcHash[:12])

	inst, err := jsruntime.New()
	if err != nil {
		return r.fail(ctx, &hook, fmt.Sprintf("engine init: %v", err))
	}
	defer inst.Close() // MVP: instance is per-reconcile until the registry lands

	if err := inst.LoadModule(req.Name+".js", string(source)); err != nil {
		log.Error(err, "evaluating module")
		return r.fail(ctx, &hook, fmt.Sprintf("module eval: %v", err))
	}

	cfg, err := inst.LoadConfig()
	if err != nil {
		log.Error(err, "calling config()")
		return r.fail(ctx, &hook, fmt.Sprintf("config(): %v", err))
	}

	bindings := summarizeBindings(cfg)
	log.Info("hook configured", "bindings", bindings)

	now := metav1.NewTime(time.Now())
	hook.Status.Phase = "Ready"
	hook.Status.ObservedGeneration = hook.Generation
	hook.Status.Bindings = bindings
	hook.Status.Instance = &corev1alpha1.JSHookInstanceStatus{
		StartedAt:  &now,
		SourceHash: srcHash,
		// RestartCount stays 0 in this MVP; tracked properly by the registry.
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

// SetupWithManager sets up the controller with the Manager.
func (r *JSHookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Loader == nil {
		r.Loader = hooks.NewChain(hooks.InlineLoader{})
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.JSHook{}).
		Named("jshook").
		Complete(r)
}
