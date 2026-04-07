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

// Package framework provides integration test utilities for the Coraza
// Kubernetes Operator.
//
// It handles cluster connection (kind or generic kubeconfig), resource
// lifecycle, status assertions, and HTTP traffic verification through
// Gateway port-forwarding.
//
// Usage:
//
//	fw, err := framework.New()
//	// ... in a test function:
//	s := fw.NewScenario(t)
//	s.CreateNamespace("my-test")
//	s.CreateRuleSource("my-test", "rules", rulesData)
//	s.CreateRuleSet("my-test", "ruleset", refs)
//	s.CreateEngine("my-test", "engine", framework.EngineOpts{...})
//	s.ExpectEngineReady("my-test", "engine")
//	gw := s.ProxyToGateway("my-test", "gateway-name")
//	gw.ExpectBlocked("/?attack=payload")
package framework

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// -----------------------------------------------------------------------------
// Vars
// -----------------------------------------------------------------------------

var (
	// portCounter allocates unique local ports for port forwarding.
	// Starts at 29000 to avoid conflicts with hard-coded ports in legacy tests.
	portCounter uint32 = 29000
)

// -----------------------------------------------------------------------------
// Framework
// -----------------------------------------------------------------------------

// Framework provides cluster access and test utilities for integration tests.
type Framework struct {
	// RestConfig is the Kubernetes REST client configuration.
	RestConfig *rest.Config

	// KubeClient is the typed Kubernetes client.
	KubeClient kubernetes.Interface

	// DynamicClient is the dynamic Kubernetes client for unstructured resources.
	DynamicClient dynamic.Interface

	// ClusterName is the cluster identifier (kind cluster name or "external").
	ClusterName string

	// IstioGatewayRevision is the value for metadata.labels["istio.io/rev"] on
	// Gateways built by BuildGateway. Empty means omit the label (default-revision
	// Istio). Set via ISTIO_GATEWAY_REVISION (e.g. "coraza" for kind, "openshift-gateway" for OCP).
	IstioGatewayRevision string
}

// New creates a Framework by detecting the cluster environment.
//
// Detection order:
//  1. KIND_CLUSTER_NAME env var: connects to a kind cluster via `kind get kubeconfig`
//  2. KUBECONFIG env var or ~/.kube/config: connects using standard kubeconfig
func New() (*Framework, error) {
	clusterName := os.Getenv("KIND_CLUSTER_NAME")

	var config *rest.Config
	var err error

	if clusterName != "" {
		cmd := exec.Command("kind", "get", "kubeconfig", "--name", clusterName)
		output, cmdErr := cmd.Output()
		if cmdErr != nil {
			return nil, fmt.Errorf("failed to get kind kubeconfig for cluster %q: %w", clusterName, cmdErr)
		}
		config, err = clientcmd.RESTConfigFromKubeConfig(output)
		if err != nil {
			return nil, fmt.Errorf("failed to parse kind kubeconfig: %w", err)
		}
	} else {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			loadingRules, &clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
		}
		clusterName = "external"
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	fw := &Framework{
		RestConfig:           config,
		KubeClient:           kubeClient,
		DynamicClient:        dynamicClient,
		ClusterName:          clusterName,
		IstioGatewayRevision: strings.TrimSpace(os.Getenv("ISTIO_GATEWAY_REVISION")),
	}

	// Set up operator metrics RBAC for integration tests
	if err := fw.setupMetricsRBAC(); err != nil {
		return nil, fmt.Errorf("failed to setup metrics RBAC: %w", err)
	}

	return fw, nil
}

// setupMetricsRBAC creates RBAC to allow the operator service account to access
// its own /metrics endpoint for integration testing. The operator already has
// tokenreviews/subjectaccessreviews permissions from the production manifests
// (needed to validate incoming tokens), so we only need to grant access to the
// /metrics non-resource URL.
func (f *Framework) setupMetricsRBAC() error {
	const (
		operatorNamespace    = "coraza-system"
		operatorSA           = "coraza-kubernetes-operator"
		metricsReaderRole    = "coraza-metrics-reader-test"
		metricsReaderBinding = "coraza-metrics-reader-test"
	)

	// ClusterRole for accessing /metrics endpoint
	metricsRole := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ` + metricsReaderRole + `
rules:
  - nonResourceURLs: ["/metrics"]
    verbs: ["get"]`

	metricsBinding := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ` + metricsReaderBinding + `
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ` + metricsReaderRole + `
subjects:
  - kind: ServiceAccount
    name: ` + operatorSA + `
    namespace: ` + operatorNamespace

	allResources := metricsRole + "\n---\n" + metricsBinding

	// Use framework's Kubectl helper to ensure correct cluster context.
	// Empty namespace because these are cluster-scoped resources.
	cmd := f.Kubectl("", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(allResources)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kubectl apply failed: %w\noutput: %s", err, string(output))
	}

	return nil
}

// CleanupMetricsRBAC removes the metrics RBAC resources created during framework initialization.
func (f *Framework) CleanupMetricsRBAC() {
	// Use framework's Kubectl helper to ensure cleanup targets the same cluster
	// context as the tests. Empty namespace because these are cluster-scoped.
	_ = f.Kubectl("", "delete", "clusterrolebinding", "coraza-metrics-reader-test").Run()
	_ = f.Kubectl("", "delete", "clusterrole", "coraza-metrics-reader-test").Run()
}
