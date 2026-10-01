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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	jsadmissionctrl "github.com/o-haase/gojsop/internal/jsadmission/controller"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jsrun"
	"github.com/o-haase/gojsop/internal/jssource"
)

var _ = Describe("JSAdmission Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-jsadmission"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{Name: resourceName}
		jsadmission := &corev1alpha1.JSAdmission{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind JSAdmission")
			err := k8sClient.Get(ctx, typeNamespacedName, jsadmission)
			if err != nil && errors.IsNotFound(err) {
				resource := &corev1alpha1.JSAdmission{
					ObjectMeta: metav1.ObjectMeta{Name: resourceName},
					Spec: corev1alpha1.JSAdmissionSpec{
						Type: "validating",
						Rules: []corev1alpha1.AdmissionRule{{
							APIGroups:   []string{""},
							APIVersions: []string{"v1"},
							Resources:   []string{"pods"},
							Operations:  []string{"CREATE"},
						}},
						FailurePolicy: "Fail",
						SideEffects:   "None",
						Source: corev1alpha1.JSSource{
							Inline: "function validate(req) { return { allowed: true }; }",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &corev1alpha1.JSAdmission{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance JSAdmission")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &jsadmissionctrl.JSAdmissionReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Loader: jssource.NewChain(jssource.InlineLoader{}),
				Runner: jsregistry.NewRegistry(),
			}

			// The first reconcile only starts the build; reconcile again until Ready.
			Eventually(func(g Gomega) {
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				g.Expect(err).NotTo(HaveOccurred())
				_, ok := controllerReconciler.Runner.Instance(jsrun.AdmissionKey(typeNamespacedName))
				g.Expect(ok).To(BeTrue())
			}, "10s", "20ms").Should(Succeed())
		})
	})
})
