package jssource

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
)

// js-sources.R4
func TestJSHookConfigMapMapper_FansOutMatchingHooks(t *testing.T) {
	hookA := &corev1alpha1.JSHook{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec: corev1alpha1.JSHookSpec{
			Source: corev1alpha1.JSSource{
				ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "shared", Namespace: "ns1"},
			},
		},
	}
	hookB := &corev1alpha1.JSHook{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: corev1alpha1.JSHookSpec{
			Source: corev1alpha1.JSSource{
				ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "other", Namespace: "ns1"},
			},
		},
	}
	hookC := &corev1alpha1.JSHook{
		ObjectMeta: metav1.ObjectMeta{Name: "c"},
		Spec: corev1alpha1.JSHookSpec{
			Source: corev1alpha1.JSSource{Inline: "function handle(){}"},
		},
	}
	c := newReader(t, hookA, hookB, hookC).Build()

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "ns1"}}
	reqs := JSHookConfigMapMapper(c)(context.Background(), cm)

	if len(reqs) != 1 {
		t.Fatalf("requests: got %d, want 1 (only hook a refs shared)", len(reqs))
	}
	if reqs[0].Name != "a" {
		t.Fatalf("request name: got %q want %q", reqs[0].Name, "a")
	}
}

// js-sources.R4
func TestJSHookConfigMapMapper_NoMatch(t *testing.T) {
	hook := &corev1alpha1.JSHook{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec: corev1alpha1.JSHookSpec{
			Source: corev1alpha1.JSSource{
				ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "shared", Namespace: "ns1"},
			},
		},
	}
	c := newReader(t, hook).Build()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "ns1"}}

	reqs := JSHookConfigMapMapper(c)(context.Background(), cm)
	if len(reqs) != 0 {
		t.Fatalf("requests: got %d, want 0 (no hook refs unrelated)", len(reqs))
	}
}

// js-sources.R4
func TestJSAdmissionConfigMapMapper_FansOutMatchingPolicies(t *testing.T) {
	pol := &corev1alpha1.JSAdmission{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: corev1alpha1.JSAdmissionSpec{
			Source: corev1alpha1.JSSource{
				ConfigMapRef: &corev1alpha1.ConfigMapKeyRef{Name: "shared", Namespace: "ns2"},
			},
		},
	}
	c := newReader(t, pol).Build()

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "ns2"}}
	reqs := JSAdmissionConfigMapMapper(c)(context.Background(), cm)

	if len(reqs) != 1 || reqs[0].Name != "p" {
		t.Fatalf("requests: %+v want exactly p", reqs)
	}
}
