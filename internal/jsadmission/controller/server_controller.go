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
	"time"

	"github.com/go-logr/logr"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlsource "sigs.k8s.io/controller-runtime/pkg/source"

	corev1alpha1 "github.com/efuturetoday/gojsop/api/v1alpha1"
	"github.com/efuturetoday/gojsop/internal/conditions"
	"github.com/efuturetoday/gojsop/internal/jsaccess"
	"github.com/efuturetoday/gojsop/internal/jsadmission"
	"github.com/efuturetoday/gojsop/internal/jsengine/kubehost"
	"github.com/efuturetoday/gojsop/internal/jsrun"
	"github.com/efuturetoday/gojsop/internal/jssource"
)

// sourceRetry spaces the retries of a policy whose source cannot be loaded.
// The leader reports the failure in the status; this controller only has to
// come back.
const sourceRetry = 5 * time.Second

// JSAdmissionServerReconciler keeps the local admission server able to answer
// for every JSAdmission: it loads the source, prepares the script and
// publishes the policy in the HTTP dispatcher.
//
// It is deliberately **not** leader-elected. The apiserver reaches the
// webhook through a Service that routes to every ready pod, so a replica that
// knows no policy answers 404 — and with failurePolicy: Fail that denies
// matching requests cluster-wide. Every replica therefore has to hold the
// whole policy table (jsadmission.R20).
//
// What it does not do: it writes no status and it does not touch the central
// WebhookConfigurations. Both have exactly one writer, the leader-only
// JSAdmissionReconciler, because N replicas writing the same object would
// fight over it.
//
// The price is that every replica builds every policy's script, so the memory
// of the admission path scales with the replica count. js-registry.R20 sizes
// it per process.
type JSAdmissionServerReconciler struct {
	client.Client

	// Loader resolves spec.source to JS bytes (defaults to inline-only).
	Loader *jssource.Chain
	// Scripts owns the per-policy prepared scripts.
	Scripts jsrun.Scripts
	// KubeHost mints the host-function surface installed on every VM.
	// ForAdmission returns a read-only binder.
	KubeHost kubehost.Factory

	// ServiceAccounts, when set, makes every policy's kube.* calls run as
	// the policy's own ServiceAccount, which the leader creates
	// (JSAdmissionReconciler.Access). Optional in tests.
	ServiceAccounts bool
	// Server holds the live policy table the HTTP webhook handler consults.
	Server *jsadmission.Server

	// Recorder publishes the lifecycle events of a review (panic, timeout,
	// script error). Nil-safe. With more than one replica the same crash can
	// be reported by each replica that saw it; every one of those events is
	// true for the replica that wrote it.
	Recorder events.EventRecorder

	// Backoff spaces the retries of a policy whose build failed.
	Backoff jsrun.Backoff
}

// event records a corev1.Event about obj. Nil-safe.
func (r *JSAdmissionServerReconciler) event(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, eventType, reason, eventAction, "%s", message)
	}
}

// Reconcile prepares the script of one policy and publishes it locally.
//
// jsadmission.R20
func (r *JSAdmissionServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("jsadmission", req.Name)
	key := jsrun.AdmissionKey(req.NamespacedName)

	var pol corev1alpha1.JSAdmission
	if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(log, req, key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// A policy on its way out stops being served here at once. The central
	// WebhookConfigurations are the leader's job; once the entry is gone the
	// apiserver stops routing here anyway, so the replicas need no agreement
	// on the moment.
	if !pol.DeletionTimestamp.IsZero() {
		r.forget(log, req, key)
		return ctrl.Result{}, nil
	}

	source, err := r.Loader.Load(ctx, pol.Spec.Source)
	if err != nil {
		// The leader turns this into SourceLoadFailed on the status; here it
		// is only a reason to come back.
		log.Error(err, "loading admission source")
		return ctrl.Result{RequeueAfter: sourceRetry}, nil
	}
	lim := admissionLimitsFromSpec(pol.Spec.Limits)
	mutating := isMutating(&pol)

	var host jsrun.Host
	if r.KubeHost != nil {
		var sa string
		if r.ServiceAccounts {
			sa = jsaccess.Name(jsrun.KindJSAdmission, req.Name)
		}
		host, err = r.KubeHost.ForAdmission(ctx, req.NamespacedName, sa)
		if err != nil {
			log.Error(err, "minting kube host")
			return ctrl.Result{RequeueAfter: sourceRetry}, nil
		}
	}

	// The build runs in the background; the registry notifies this controller
	// when it ends (SetupWithManager), so a reconcile never waits for it.
	// js-registry.R15
	st := r.Scripts.Ensure(key, jsrun.Spec{
		Source:     source,
		SourceHash: jssource.Hash(source),
		Limits:     lim,
		Host:       host,
		PostBuild:  admissionPostBuild(mutating),
		ResetToken: pol.GetAnnotations()[conditions.ManualRestartAnnotation],
		Backoff:    r.Backoff,
	})
	switch st.Phase {
	case jsrun.PhasePreparing:
		// An older script, if there is one, keeps serving until the new one
		// is installed (js-registry.R19), so the registration stays as it is.
		log.V(1).Info("instance building")
		return ctrl.Result{}, nil
	case jsrun.PhaseFailed:
		log.Error(st.Err, "instance build failed", "attempts", st.Attempts, "retryIn", st.RetryIn())
		return ctrl.Result{RequeueAfter: st.RetryIn()}, nil
	}

	// Snapshot the policy meta into the closure so the recorder has a stable
	// target for the lifetime of this registration.
	polForEvents := pol.DeepCopy()
	r.Server.Register(jsadmission.PolicyEntry{
		Key:           req.NamespacedName,
		Mutating:      mutating,
		Timeout:       callTimeout(pol.Spec.TimeoutSeconds, lim),
		FailurePolicy: admissionregv1.FailurePolicyType(pol.Spec.FailurePolicy),
		Emit: func(eventType, reason, message string) {
			r.event(polForEvents, eventType, reason, message)
		},
	})
	log.V(1).Info("policy published locally", "hash", st.SourceHash[:min(12, len(st.SourceHash))])
	return ctrl.Result{}, nil
}

// forget stops serving key on this replica and releases its script.
func (r *JSAdmissionServerReconciler) forget(log logr.Logger, req ctrl.Request, key jsrun.Key) {
	log.V(1).Info("cleanup started", "phase", "delete")
	if r.Server != nil {
		r.Server.Unregister(req.NamespacedName)
	}
	r.Scripts.Drop(key)
	log.V(1).Info("cleanup done", "phase", "delete")
}

// everyReplicaRunnable is a manager Runnable that runs on every replica, not
// only on the leader. manager.RunnableFunc does not implement
// LeaderElectionRunnable, so the manager would put it in the leader group.
type everyReplicaRunnable struct {
	fn func(context.Context) error
}

func (e everyReplicaRunnable) Start(ctx context.Context) error { return e.fn(ctx) }

// NeedLeaderElection keeps the runnable out of the leader-only group.
func (e everyReplicaRunnable) NeedLeaderElection() bool { return false }

// serverControllerOptions keeps this controller out of the leader-elected
// group. Without it the controller would be the manager's default — leader
// only — and every other replica would answer 404.
//
// jsadmission.R20
func serverControllerOptions() ctrlcontroller.Options {
	notLeaderElected := false
	return ctrlcontroller.Options{NeedLeaderElection: &notLeaderElected}
}

// SetupWithManager registers the controller without leader election.
//
// jsadmission.R20
func (r *JSAdmissionServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Loader == nil {
		r.Loader = jssource.NewChain(jssource.InlineLoader{})
	}
	if r.Scripts == nil {
		return errors.New("jsrun.Scripts is required")
	}
	if r.Server == nil {
		return errors.New("jsadmission.Server is required: without it no replica answers an admission request")
	}
	// kube-access.R8
	if r.KubeHost == nil {
		return errors.New("kubehost.Factory is required: without it policies get no kube global")
	}

	// The registry reports every finished build of a JSAdmission. This
	// forwarder has to run on every replica too, so it cannot be a plain
	// RunnableFunc.
	ch := make(chan event.TypedGenericEvent[*corev1alpha1.JSAdmission], 64)
	if err := mgr.Add(everyReplicaRunnable{fn: func(ctx context.Context) error {
		for key := range r.Scripts.Watch(ctx, jsrun.KindJSAdmission) {
			pol := &corev1alpha1.JSAdmission{ObjectMeta: metav1.ObjectMeta{Name: key.Name.Name, Namespace: key.Name.Namespace}}
			select {
			case ch <- event.TypedGenericEvent[*corev1alpha1.JSAdmission]{Object: pol}:
			case <-ctx.Done():
				return nil
			}
		}
		return nil
	}}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.JSAdmission{}).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(jssource.JSAdmissionConfigMapMapper(mgr.GetClient())),
		).
		WatchesRawSource(ctrlsource.Channel(ch, &handler.TypedEnqueueRequestForObject[*corev1alpha1.JSAdmission]{})).
		WithOptions(serverControllerOptions()).
		Named("jsadmission-server").
		Complete(r)
}
