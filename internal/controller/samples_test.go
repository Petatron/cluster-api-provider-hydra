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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// The shipped samples are the first thing anyone runs against this provider, and
// they previously did not validate at all -- spec was written as a comment, which
// YAML reads as null against a schema that requires an object. Applying them here
// means the schema and the samples cannot drift apart silently again.
var _ = Describe("config/samples", func() {
	ctx := context.Background()

	It("every sample applies against the generated CRDs", func() {
		dir := filepath.Join("..", "..", "config", "samples")
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())

		applied := 0
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" || e.Name() == "kustomization.yaml" {
				continue
			}

			By("applying " + e.Name())
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			Expect(err).NotTo(HaveOccurred())

			obj := &unstructured.Unstructured{}
			Expect(yaml.Unmarshal(raw, obj)).To(Succeed(), e.Name())
			obj.SetNamespace("default")

			Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "%s failed schema validation", e.Name())
			Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
			applied++
		}

		Expect(applied).To(BeNumerically(">", 0), "no samples were found to validate")
	})
})
