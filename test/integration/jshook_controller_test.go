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
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
	"github.com/o-haase/gojsop/internal/conditions"
	jshookctrl "github.com/o-haase/gojsop/internal/jshook/controller"
	"github.com/o-haase/gojsop/internal/jsregistry"
	"github.com/o-haase/gojsop/internal/jssource"
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
						Source: corev1alpha1.JSSource{
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
							}
							function handle() {}`,
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
			controllerReconciler := &jshookctrl.JSHookReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Loader: jssource.NewChain(jssource.InlineLoader{}),
				Runner: jsregistry.NewRegistry(),
			}

			// The first reconcile only starts the build and reports it
			// (jshook.R18); reconcile again until the VM is Ready.
			updated := &corev1alpha1.JSHook{}
			Eventually(func(g Gomega) {
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
				cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditions.Ready)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}, "10s", "20ms").Should(Succeed())
			Expect(updated.Status.Bindings).To(ContainElement("kubernetes:v1/ConfigMap/watch-cm"))
			Expect(updated.Status.Bindings).To(ContainElement("onStartup:5"))
			Expect(updated.Status.Instance).NotTo(BeNil())
			Expect(updated.Status.Instance.SourceHash).NotTo(BeEmpty())
		})
	})

	DescribeTable("When the hook source lacks a required export",
		func(name, inline, missing string) {
			// jshook.R2
			ctx := context.Background()
			nn := types.NamespacedName{Name: name}
			Expect(k8sClient.Create(ctx, &corev1alpha1.JSHook{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec:       corev1alpha1.JSHookSpec{Source: corev1alpha1.JSSource{Inline: inline}},
			})).To(Succeed())
			DeferCleanup(func() {
				hook := &corev1alpha1.JSHook{}
				Expect(k8sClient.Get(ctx, nn, hook)).To(Succeed())
				Expect(k8sClient.Delete(ctx, hook)).To(Succeed())
			})

			recorder := events.NewFakeRecorder(10)
			reg := jsregistry.NewRegistry()
			reconciler := &jshookctrl.JSHookReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Loader:   jssource.NewChain(jssource.InlineLoader{}),
				Runner:   reg,
				Recorder: recorder,
			}
			// jshook.R18: while the build runs the hook is Ready=False/Building.
			res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())
			updated := &corev1alpha1.JSHook{}
			Expect(k8sClient.Get(ctx, nn, updated)).To(Succeed())
			cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditions.Ready)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(conditions.ReasonBuilding))

			// jshook.R18: a failed build is Ready=False/BuildFailed and requeues with backoff.
			Eventually(func(g Gomega) {
				res, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx, nn, updated)).To(Succeed())
				cond = apimeta.FindStatusCondition(updated.Status.Conditions, conditions.Ready)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Reason).To(Equal(conditions.ReasonBuildFailed))
			}, "10s", "20ms").Should(Succeed())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Message).To(ContainSubstring("missing required export: " + missing + "()"))
			Expect(recorder.Events).To(Receive(ContainSubstring(conditions.EventEntrypointMissing)))
			Expect(reg.Len()).To(BeZero())
		},
		Entry("no handle", "no-handle-hook", `function config() { return {}; }`, "handle"),
		Entry("no config", "no-config-hook", `function handle() {}`, "config"),
	)
})
