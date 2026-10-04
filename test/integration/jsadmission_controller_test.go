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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	jsadmissionsrv "github.com/o-haase/gojsop/internal/jsadmission"
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
							ResourceRule: corev1alpha1.ResourceRule{
								APIGroups:   []string{""},
								APIVersions: []string{"v1"},
								Resources:   []string{"pods"},
							},
							Operations: []string{"CREATE"},
						}},
						FailurePolicy: "Fail",
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
		// Two replicas against one apiserver: each one prepares the script
		// and publishes the policy for itself, and the status is written
		// once, by the leader-only reconciler.
		//
		// jsadmission.R20
		It("serves the policy on every replica and reports it once", func() {
			By("Reconciling the created resource on two replicas")
			replica := func() *jsadmissionctrl.JSAdmissionServerReconciler {
				registry := jsregistry.NewRegistry()
				DeferCleanup(func() { registry.Drop(jsrun.AdmissionKey(typeNamespacedName)) })
				return &jsadmissionctrl.JSAdmissionServerReconciler{
					Client:   k8sClient,
					Loader:   jssource.NewChain(jssource.InlineLoader{}),
					Scripts:  registry,
					Server:   jsadmissionsrv.NewServer(registry, logf.Log),
					KubeHost: nil,
				}
			}
			replicas := []*jsadmissionctrl.JSAdmissionServerReconciler{replica(), replica()}
			leader := &jsadmissionctrl.JSAdmissionReconciler{
				Client:  k8sClient,
				Scheme:  k8sClient.Scheme(),
				Loader:  jssource.NewChain(jssource.InlineLoader{}),
				Scripts: replicas[0].Scripts,
			}

			// The first reconcile only starts the build; reconcile again until Ready.
			Eventually(func(g Gomega) {
				for _, r := range replicas {
					_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
					g.Expect(err).NotTo(HaveOccurred())
					st, known := r.Scripts.State(jsrun.AdmissionKey(typeNamespacedName))
					g.Expect(known).To(BeTrue())
					g.Expect(st.Phase).To(Equal(jsrun.PhaseReady))
				}
			}, "20s", "20ms").Should(Succeed())

			By("Reporting the policy Ready from the leader-only reconciler")
			Eventually(func(g Gomega) {
				_, err := leader.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
				g.Expect(err).NotTo(HaveOccurred())
				var got corev1alpha1.JSAdmission
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, &got)).To(Succeed())
				cond := apimeta.FindStatusCondition(got.Status.Conditions, conditions.Ready)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}, "10s", "20ms").Should(Succeed())
		})
	})
})
