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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// api-design.R8
var _ = Describe("config/samples", func() {
	It("applies every sample against the generated CRDs", func() {
		ctx := context.Background()
		dir := filepath.Join("..", "..", "config", "samples")

		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())

		applied := 0
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || filepath.Ext(name) != ".yaml" || name == "kustomization.yaml" {
				continue
			}
			By("applying " + name)
			raw, err := os.ReadFile(filepath.Join(dir, name))
			Expect(err).NotTo(HaveOccurred())

			obj := &unstructured.Unstructured{}
			Expect(yaml.Unmarshal(raw, &obj.Object)).To(Succeed(), name)
			Expect(k8sClient.Create(ctx, obj, client.DryRunAll)).To(Succeed(), name)
			applied++
		}
		Expect(applied).To(BeNumerically(">", 0), "no sample found in config/samples")
	})
})
