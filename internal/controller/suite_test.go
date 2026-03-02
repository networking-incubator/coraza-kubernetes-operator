/*
Copyright Coraza Kubernetes Operator contributors.

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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Envtest Suite - Vars
// -----------------------------------------------------------------------------

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	scheme    *runtime.Scheme
)

// -----------------------------------------------------------------------------
// Envtest Suite - Main
// -----------------------------------------------------------------------------

func TestMain(m *testing.M) {
	istioCRDDir, err := downloadIstioCRDs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to download Istio CRDs: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if rmErr := os.RemoveAll(istioCRDDir); rmErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to cleanup Istio CRD dir: %v\n", rmErr)
		}
	}()

	scheme = runtime.NewScheme()
	if err := wafv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to add waf scheme: %v\n", err)
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to add core scheme: %v\n", err)
		os.Exit(1)
	}

	// The version used here MUST reflect the available versions at
	// controller-runtime repo: https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/HEAD/envtest-releases.yaml
	// If the envvar is not passed, the latest GA will be used
	k8sVersion := os.Getenv("K8S_VERSION")

	testEnv = &envtest.Environment{
		CRDInstallOptions: envtest.CRDInstallOptions{
			Paths: []string{
				filepath.Join("..", "..", "config", "crd", "bases"),
				istioCRDDir,
			},
			CleanUpAfterUse: true,
		},
		Scheme:                      scheme,
		DownloadBinaryAssets:        true,
		DownloadBinaryAssetsVersion: k8sVersion,
		ErrorIfCRDPathMissing:       true,
	}
	cfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start test environment: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to stop test environment: %v\n", err)
			os.Exit(1)
		}
	}()

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stop test environment: %v\n", err)
		os.Exit(1)
	}

	os.Exit(code)
}

// -----------------------------------------------------------------------------
// Envtest Suite - Helpers
// -----------------------------------------------------------------------------

func setupTest(t *testing.T) (context.Context, func()) {
	ctx := context.Background()

	ns := &corev1.Namespace{}
	ns.Name = fmt.Sprintf("envtest-%s", uuid.New().String())
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("Failed to create test namespace: %v", err)
	}

	cleanup := func() {
		if err := k8sClient.Delete(ctx, ns); err != nil {
			t.Logf("Failed to delete test namespace: %v", err)
		}
	}

	return ctx, cleanup
}

func downloadIstioCRDs() (string, error) {
	istioVersion := os.Getenv("ISTIO_VERSION")
	if istioVersion == "" {
		return "", errors.New("ISTIO_VERSION environment variable is required")
	}

	tmpDir, err := os.MkdirTemp("", "istio-crds-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	wasmPluginURL := fmt.Sprintf("https://raw.githubusercontent.com/istio/istio/refs/tags/%s/manifests/charts/base/files/crd-all.gen.yaml", istioVersion)
	resp, err := http.Get(wasmPluginURL)
	if err != nil {
		return "", fmt.Errorf("failed to download Istio CRDs: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close response body: %v\n", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download Istio CRDs: HTTP %d", resp.StatusCode)
	}

	crdFile := filepath.Join(tmpDir, "istio-crds.yaml")
	f, err := os.Create(crdFile)
	if err != nil {
		return "", fmt.Errorf("failed to create CRD file: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close file: %v\n", closeErr)
		}
	}()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("failed to write CRD file: %w", err)
	}

	return tmpDir, nil
}
