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

package integration

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
)

var _ = Describe("spec.permissions", func() {
	ctx := context.Background()
	core := []string{""}
	v1 := []string{"v1"}
	perm := func(resource string, verbs ...corev1alpha1.PermissionVerb) corev1alpha1.Permission {
		return corev1alpha1.Permission{APIGroups: []string{""}, Resources: []string{resource}, Verbs: verbs}
	}
	policy := func(p corev1alpha1.Permission) *corev1alpha1.JSAdmission {
		return &corev1alpha1.JSAdmission{
			ObjectMeta: metav1.ObjectMeta{Name: "perm-check"},
			Spec: corev1alpha1.JSAdmissionSpec{
				Source: corev1alpha1.JSSource{Inline: "function validate() { return { allowed: true }; }"},
				Rules: []corev1alpha1.AdmissionRule{{
					ResourceRule: corev1alpha1.ResourceRule{APIGroups: core, APIVersions: v1, Resources: []string{"pods"}},
					Operations:   []string{"CREATE"},
				}},
				Permissions: []corev1alpha1.Permission{p},
			},
		}
	}
	hook := func(p corev1alpha1.Permission) *corev1alpha1.JSHook {
		return &corev1alpha1.JSHook{
			ObjectMeta: metav1.ObjectMeta{Name: "perm-check"},
			Spec: corev1alpha1.JSHookSpec{
				Source: corev1alpha1.JSSource{Inline: "function handle() {}"},
				Bindings: []corev1alpha1.HookBinding{{
					Name:         "cms",
					ResourceRule: corev1alpha1.ResourceRule{APIGroups: core, APIVersions: v1, Resources: []string{"configmaps"}},
				}},
				Permissions: []corev1alpha1.Permission{p},
			},
		}
	}

	It("lets a policy only read", func() {
		// kube-access.R14
		Expect(k8sClient.Create(ctx, policy(perm("configmaps", "get", "list")), client.DryRunAll)).To(Succeed())
		Expect(k8sClient.Create(ctx, policy(perm("configmaps", "get", "update")), client.DryRunAll)).
			To(MatchError(ContainSubstring("a policy can only read")))
	})

	It("rejects wildcards", func() {
		// kube-access.R14
		Expect(k8sClient.Create(ctx, hook(perm("configmaps", "get", "update")), client.DryRunAll)).To(Succeed())
		Expect(k8sClient.Create(ctx, hook(perm("*", "get")), client.DryRunAll)).NotTo(Succeed())
		star := perm("configmaps", "get")
		star.APIGroups = []string{"*"}
		Expect(k8sClient.Create(ctx, hook(star), client.DryRunAll)).NotTo(Succeed())
		Expect(k8sClient.Create(ctx, hook(perm("configmaps", "*")), client.DryRunAll)).NotTo(Succeed())
	})
})
