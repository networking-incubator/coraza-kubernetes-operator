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
//	s.CreateConfigMap("my-test", "rules", rulesData)
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
	"sync/atomic"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// portCounter allocates unique local ports for port forwarding.
// Starts at 29000 to avoid conflicts with hard-coded ports in legacy tests.
var portCounter uint32 = 29000

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

	return &Framework{
		RestConfig:    config,
		KubeClient:    kubeClient,
		DynamicClient: dynamicClient,
		ClusterName:   clusterName,
	}, nil
}

// AllocatePort returns the next available local port for port forwarding.
func AllocatePort() string {
	port := atomic.AddUint32(&portCounter, 1) - 1
	return fmt.Sprintf("%d", port)
}

// KubeContext returns the kubectl context string for the cluster.
// For kind clusters returns "kind-<name>". For external clusters returns "".
func (f *Framework) KubeContext() string {
	if f.ClusterName == "external" {
		return ""
	}
	return fmt.Sprintf("kind-%s", f.ClusterName)
}

// Kubectl returns an exec.Cmd for running kubectl against the cluster
// in the given namespace.
func (f *Framework) Kubectl(namespace string, args ...string) *exec.Cmd {
	return exec.Command("kubectl", f.kubectlArgs(namespace, args...)...)
}

func (f *Framework) kubectlArgs(namespace string, args ...string) []string {
	cmdArgs := make([]string, 0, len(args)+4)
	if ctx := f.KubeContext(); ctx != "" {
		cmdArgs = append(cmdArgs, "--context", ctx)
	}
	if namespace != "" {
		cmdArgs = append(cmdArgs, "-n", namespace)
	}
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
}
