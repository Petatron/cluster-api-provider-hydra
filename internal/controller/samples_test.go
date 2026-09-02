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

// controller-gen writes every CRD into config/crd/bases, but kustomize only
// deploys the ones config/crd/kustomization.yaml names. Adding an API and
// forgetting that line produces a provider whose CRD generates, validates and
// passes every test here -- because envtest loads the whole bases directory --
// and is then simply absent after `make deploy`.
//
// That gap is invisible to every other test in this suite, which is exactly why
// it is worth one of its own.
var _ = Describe("config/crd", func() {
	It("deploys every generated CRD", func() {
		bases, err := os.ReadDir(filepath.Join("..", "..", "config", "crd", "bases"))
		Expect(err).NotTo(HaveOccurred())

		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "kustomization.yaml"))
		Expect(err).NotTo(HaveOccurred())
		kustomization := string(raw)

		generated := 0
		for _, e := range bases {
			if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
				continue
			}
			generated++
			Expect(kustomization).To(ContainSubstring("bases/"+e.Name()),
				"%s is generated but not listed in config/crd/kustomization.yaml, so `make deploy` would omit it", e.Name())
		}
		Expect(generated).To(BeNumerically(">", 0), "no generated CRDs were found to check")
	})
})
