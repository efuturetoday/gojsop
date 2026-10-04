//go:build e2e
// +build e2e

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

package e2e

import (
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/efuturetoday/gojsop/test/utils"
)

// e2eNamespace holds the objects the scenarios create; e2eTarget receives
// the copies of the ConfigMap sync hook.
const (
	e2eNamespace = "gojsop-e2e"
	e2eTarget    = "gojsop-e2e-target"
	// e2eUser may create hooks but holds no other right.
	e2eUser = "e2e-hook-author"
)

func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

// apply pipes a manifest into kubectl apply; extra args go before "-f -".
func apply(manifest string, extra ...string) (string, error) {
	cmd := exec.Command("kubectl", append(append([]string{"apply"}, extra...), "-f", "-")...)
	cmd.Stdin = strings.NewReader(manifest)
	return utils.Run(cmd)
}

// jsonpath runs kubectl and trims the output, for Eventually and Consistently.
func jsonpath(args ...string) func() (string, error) {
	return func() (string, error) {
		out, err := kubectl(args...)
		return strings.TrimSpace(out), err
	}
}

// productScenarios run what a user does with gojsop on the deployed
// operator: a hook that syncs ConfigMaps, the rights of a hook, and
// admission policies that the apiserver calls over TLS. They run inside the
// ordered "Manager" container, after the operator is up.
func productScenarios() {
	Context("gojsop", Ordered, func() {
		BeforeAll(func() {
			for _, ns := range []string{e2eNamespace, e2eTarget} {
				_, err := kubectl("create", "ns", ns)
				Expect(err).NotTo(HaveOccurred())
			}
			By("waiting for the webhooks to answer")
			Eventually(jsonpath("get", "endpointslices.discovery.k8s.io", "-n", namespace,
				"-l", "kubernetes.io/service-name=gojsop-webhook-service",
				"-o", "jsonpath={.items[*].endpoints[*].addresses[*]}")).ShouldNot(BeEmpty())
		})

		AfterAll(func() {
			// Policies first: a policy whose operator is gone would deny
			// every pod under failurePolicy: Fail.
			_, _ = kubectl("delete", "jsadmissions", "--all", "--wait=true")
			_, _ = kubectl("delete", "jshooks", "--all", "--wait=true")
			_, _ = kubectl("delete", "clusterrolebinding,clusterrole", e2eUser, "--ignore-not-found")
			_, _ = kubectl("delete", "ns", e2eNamespace, e2eTarget, "--ignore-not-found")
		})

		It("syncs a ConfigMap through a hook that acts as its own ServiceAccount", func() {
			By("creating a ConfigMap before the hook exists")
			_, err := apply(`apiVersion: v1
kind: ConfigMap
metadata:
  name: existing
  namespace: ` + e2eNamespace + `
  labels:
    gojsop.io/sync: "true"
  annotations:
    gojsop.io/sync-to: ` + e2eTarget + `
data:
  greeting: early
`)
			Expect(err).NotTo(HaveOccurred())

			By("applying the ConfigMap sync sample")
			_, err = kubectl("apply", "-f", "config/samples/core_v1alpha1_jshook.yaml")
			Expect(err).NotTo(HaveOccurred())
			Eventually(jsonpath("get", "jshook", "configmap-sync",
				"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")).Should(Equal("True"))
			Expect(jsonpath("get", "jshook", "configmap-sync", "-o", "jsonpath={.status.serviceAccount}")()).
				To(Equal("jshook-configmap-sync"))

			By("creating a ConfigMap that asks to be copied")
			_, err = apply(`apiVersion: v1
kind: ConfigMap
metadata:
  name: shared
  namespace: ` + e2eNamespace + `
  labels:
    gojsop.io/sync: "true"
  annotations:
    gojsop.io/sync-to: ` + e2eTarget + `
data:
  greeting: hello
`)
			Expect(err).NotTo(HaveOccurred())

			// The watch and kube.apply both run as jshook-configmap-sync,
			// so the copy shows that impersonation and its rights work.
			Eventually(jsonpath("get", "configmap", "shared", "-n", e2eTarget,
				"-o", "jsonpath={.data.greeting}")).Should(Equal("hello"))
			// The ConfigMap from before the hook arrived as an initial Added.
			Eventually(jsonpath("get", "configmap", "existing", "-n", e2eTarget,
				"-o", "jsonpath={.data.greeting}")).Should(Equal("early"))
		})

		It("stops a script at the edge of its rights", func() {
			By("applying a hook that tries to delete what it may only watch")
			_, err := apply(`apiVersion: core.gojsop.io/v1alpha1
kind: JSHook
metadata:
  name: e2e-no-delete
spec:
  bindings:
    - name: victims
      apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["configmaps"]
      objectSelector:
        matchLabels:
          gojsop.io/e2e-victim: "true"
  source:
    inline: |
      function handle(event) {
        const m = event.object.metadata;
        kube.delete({ apiVersion: "v1", kind: "ConfigMap", namespace: m.namespace, name: m.name });
      }
`)
			Expect(err).NotTo(HaveOccurred())
			Eventually(jsonpath("get", "jshook", "e2e-no-delete",
				"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")).Should(Equal("True"))

			_, err = kubectl("create", "configmap", "victim", "-n", e2eNamespace)
			Expect(err).NotTo(HaveOccurred())
			_, err = kubectl("label", "configmap", "victim", "-n", e2eNamespace, "gojsop.io/e2e-victim=true")
			Expect(err).NotTo(HaveOccurred())

			By("seeing the call fail on the missing right and the ConfigMap stay")
			Eventually(func() (string, error) {
				out, err := kubectl("logs", "-n", namespace, "-l", "control-plane=controller-manager", "--tail=-1")
				var hits []string
				for _, line := range strings.Split(out, "\n") {
					if strings.Contains(line, "e2e-no-delete") && strings.Contains(line, "handle() failed") {
						hits = append(hits, line)
					}
				}
				return strings.Join(hits, "\n"), err
			}).Should(ContainSubstring("forbidden"))
			Eventually(jsonpath("get", "events", "-A",
				"--field-selector", "involvedObject.name=e2e-no-delete,reason=HandleFailed",
				"-o", "jsonpath={.items[*].reason}")).Should(ContainSubstring("HandleFailed"))
			Consistently(jsonpath("get", "configmap", "victim", "-n", e2eNamespace, "-o", "name"),
				10*time.Second, time.Second).Should(Equal("configmap/victim"))
		})

		It("refuses a hook whose rights its author does not hold", func() {
			By("letting a user write hooks, and nothing else")
			_, err := apply(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ` + e2eUser + `
rules:
  - apiGroups: ["core.gojsop.io"]
    resources: ["jshooks"]
    verbs: ["get", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ` + e2eUser + `
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ` + e2eUser + `
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: ` + e2eUser + `
`)
			Expect(err).NotTo(HaveOccurred())

			out, err := apply(`apiVersion: core.gojsop.io/v1alpha1
kind: JSHook
metadata:
  name: e2e-secret-reader
spec:
  bindings:
    - name: cms
      apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["configmaps"]
  permissions:
    - apiGroups: [""]
      resources: ["secrets"]
      verbs: ["get"]
  source:
    inline: "function handle() {}"
`, "--as="+e2eUser)
			Expect(err).To(HaveOccurred())
			Expect(out).To(ContainSubstring("would get rights you do not have"))
			Expect(out).To(ContainSubstring("get secrets"))
		})

		It("removes a hook's ServiceAccount and rights with the hook", func() {
			_, err := kubectl("delete", "jshook", "e2e-no-delete", "--wait=true")
			Expect(err).NotTo(HaveOccurred())
			for _, obj := range [][]string{
				{"serviceaccount", "jshook-e2e-no-delete", "-n", namespace},
				{"clusterrole", "jshook-e2e-no-delete"},
				{"clusterrolebinding", "jshook-e2e-no-delete"},
			} {
				Eventually(func() (string, error) {
					return kubectl(append(append([]string{"get"}, obj...), "--ignore-not-found", "-o", "name")...)
				}).Should(BeEmpty(), strings.Join(obj, " "))
			}
		})

		It("rejects and changes pods through admission policies over TLS", func() {
			for _, f := range []string{"core_v1alpha1_jsadmission_validating.yaml", "core_v1alpha1_jsadmission_mutating.yaml"} {
				_, err := kubectl("apply", "-f", "config/samples/"+f)
				Expect(err).NotTo(HaveOccurred())
			}
			for _, name := range []string{"prevent-latest-tags", "add-team-label"} {
				Eventually(jsonpath("get", "jsadmission", name,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")).Should(Equal("True"), name)
			}

			By("denying an image with :latest")
			Eventually(func() (string, error) {
				out, _ := kubectl("run", "latest", "-n", e2eNamespace, "--image=busybox:latest", "--dry-run=server")
				return out, nil
			}).Should(ContainSubstring("uses :latest"))

			By("labelling an allowed pod")
			Eventually(jsonpath("run", "pinned", "-n", e2eNamespace, "--image=busybox:1.36",
				"--dry-run=server", "-o", "jsonpath={.metadata.labels.team}")).Should(Equal("frontend"))

			By("leaving kube-system alone")
			Expect(jsonpath("run", "latest", "-n", "kube-system", "--image=busybox:latest",
				"--dry-run=server", "-o", "name")()).To(Equal("pod/latest"))
		})
	})
}
