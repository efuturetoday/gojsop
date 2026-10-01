package jssource

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
)

// JSHookConfigMapMapper returns a handler.MapFunc that fans a ConfigMap event
// out to every JSHook whose spec.source.configMapRef points at it. Wire via
//
//	ctrl.NewControllerManagedBy(mgr).
//	    For(&corev1alpha1.JSHook{}).
//	    Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(jssource.JSHookConfigMapMapper(c))).
//	    Complete(r)
//
// The list is unindexed: each ConfigMap event walks the JSHook list. Cheap at
// realistic operator scale (tens of hooks); add a field index later if a
// large fleet ever shows up in profiles.
// js-sources.R4
func JSHookConfigMapMapper(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return mapConfigMapToCRs(ctx, c, obj, &corev1alpha1.JSHookList{})
	}
}

// JSAdmissionConfigMapMapper is the JSAdmission counterpart to
// JSHookConfigMapMapper. Same shape, different list type.
// js-sources.R4
func JSAdmissionConfigMapMapper(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return mapConfigMapToCRs(ctx, c, obj, &corev1alpha1.JSAdmissionList{})
	}
}

// mapConfigMapToCRs lists CRs of the given list type and returns reconcile
// requests for every one whose configMapRef matches obj. Generic over the
// two list types so the mapping logic lives in one place.
func mapConfigMapToCRs(ctx context.Context, c client.Client, obj client.Object, list client.ObjectList) []reconcile.Request {
	if obj == nil {
		return nil
	}
	cmNS := obj.GetNamespace()
	cmName := obj.GetName()
	if err := c.List(ctx, list); err != nil {
		// Returning nil drops the event; the next reconcile (or a later edit)
		// recovers. Logging is the manager's job — handler.MapFunc has no
		// logger plumbed.
		return nil
	}
	out := make([]reconcile.Request, 0, 4)
	switch ll := list.(type) {
	case *corev1alpha1.JSHookList:
		for i := range ll.Items {
			if matchesCM(ll.Items[i].Spec.Source.ConfigMapRef, cmNS, cmName) {
				out = append(out, requestFor(&ll.Items[i]))
			}
		}
	case *corev1alpha1.JSAdmissionList:
		for i := range ll.Items {
			if matchesCM(ll.Items[i].Spec.Source.ConfigMapRef, cmNS, cmName) {
				out = append(out, requestFor(&ll.Items[i]))
			}
		}
	}
	return out
}

func matchesCM(ref *corev1alpha1.ConfigMapKeyRef, ns, name string) bool {
	return ref != nil && ref.Namespace == ns && ref.Name == name
}

func requestFor(o runtime.Object) reconcile.Request {
	mo, _ := o.(client.Object)
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: mo.GetNamespace(), Name: mo.GetName()}}
}
