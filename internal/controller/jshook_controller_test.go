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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/hooks"
	jsruntime "github.com/o-haase/gojsop/internal/runtime"
)

var _ = Describe("JSHook Controller", func() {
	Context("When reconciling an inline hook", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		// JSHook is cluster-scoped, so no Namespace.
		typeNamespacedName := types.NamespacedName{Name: resourceName}
		jshook := &corev1alpha1.JSHook{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind JSHook")
			err := k8sClient.Get(ctx, typeNamespacedName, jshook)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.JSHook{
					ObjectMeta: metav1.ObjectMeta{Name: resourceName},
					Spec: corev1alpha1.JSHookSpec{
						Source: corev1alpha1.JSHookSource{
							Inline: `function config() {
								return {
									configVersion: "v1",
									onStartup: 5,
									kubernetes: [{
										name: "watch-cm",
										apiVersion: "v1",
										kind: "ConfigMap",
										executeHookOnEvent: ["Added"],
									}],
								};
							}`,
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.JSHook{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance JSHook")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should reconcile and write resolved bindings into status", func() {
			By("Reconciling the created resource")
			controllerReconciler := &JSHookReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Loader:   hooks.NewChain(hooks.InlineLoader{}),
				Registry: jsruntime.NewRegistry(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &corev1alpha1.JSHook{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal("Ready"))
			Expect(updated.Status.Bindings).To(ContainElement("kubernetes:v1/ConfigMap/watch-cm"))
			Expect(updated.Status.Bindings).To(ContainElement("onStartup:5"))
			Expect(updated.Status.Instance).NotTo(BeNil())
			Expect(updated.Status.Instance.SourceHash).NotTo(BeEmpty())
		})
	})
})
