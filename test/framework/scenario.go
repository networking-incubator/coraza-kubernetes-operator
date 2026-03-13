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

package framework

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultTimeout is the default timeout for polling operations.
	DefaultTimeout = 60 * time.Second

	// DefaultInterval is the default polling interval.
	DefaultInterval = 2 * time.Second
)

// Scenario manages the lifecycle of a single test scenario: namespace
// creation, resource cleanup, and step tracking.
//
// Cleanup is registered automatically via t.Cleanup when the Scenario
// is created — there is no need to defer it manually.
//
// Usage:
//
//	s := fw.NewScenario(t)
//	ns := s.GenerateNamespace("my-test")
//	s.Step("do something")
//	// ... test logic ...
type Scenario struct {
	T          *testing.T
	F          *Framework
	cleanups   []func()
	namespaces []string
}

// NewScenario creates a Scenario bound to the given test. Cleanup is
// registered automatically via t.Cleanup and runs after the test (and
// all its subtests) complete. On failure, diagnostic info is dumped
// before cleanup runs.
func (f *Framework) NewScenario(t *testing.T) *Scenario {
	t.Helper()
	s := &Scenario{
		T: t,
		F: f,
	}
	// t.Cleanup is LIFO: last registered runs first. Register resource
	// cleanup first (runs last) then dump (runs first, while resources
	// still exist).
	t.Cleanup(s.Cleanup)
	t.Cleanup(s.dumpOnFailure)
	return s
}

// Cleanup runs all registered cleanup functions in reverse order.
// It is idempotent: subsequent calls are no-ops.
func (s *Scenario) Cleanup() {
	cleanups := s.cleanups
	s.cleanups = nil
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
}

// OnCleanup registers a function to run during Cleanup (LIFO order).
func (s *Scenario) OnCleanup(fn func()) {
	s.cleanups = append(s.cleanups, fn)
}

// Step logs a named step in the test output for readability.
func (s *Scenario) Step(name string) {
	s.T.Helper()
	s.T.Logf("--- step: %s", name)
}

// GenerateNamespace creates a namespace with a random 6-hex-char suffix
// appended to prefix (e.g. "my-test-a1b2c3") and registers it for cleanup.
// Returns the generated name for use in subsequent resource calls.
func (s *Scenario) GenerateNamespace(prefix string) string {
	s.T.Helper()
	b := make([]byte, 3)
	_, err := rand.Read(b)
	require.NoError(s.T, err, "generate random suffix")
	name := fmt.Sprintf("%s-%x", prefix, b)
	s.CreateNamespace(name)
	return name
}

// CreateNamespace creates a namespace and registers it for cleanup.
func (s *Scenario) CreateNamespace(name string) {
	s.T.Helper()
	ctx := s.T.Context()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	_, err := s.F.KubeClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	require.NoError(s.T, err, "create namespace %s", name)

	s.namespaces = append(s.namespaces, name)
	s.T.Logf("Created namespace: %s", name)
	s.OnCleanup(func() {
		// Background: test context may already be cancelled; cleanup must still run.
		if err := s.F.KubeClient.CoreV1().Namespaces().Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete namespace %s: %v", name, err)
		}
	})
}

// ApplyManifest applies a YAML manifest file via kubectl and registers
// cleanup to delete it.
func (s *Scenario) ApplyManifest(namespace, path string) {
	s.T.Helper()
	out, err := s.F.Kubectl(namespace, "apply", "-f", path).CombinedOutput()
	require.NoError(s.T, err, "apply manifest %s: %s", path, string(out))

	s.T.Logf("Applied manifest: %s", path)
	s.OnCleanup(func() {
		_ = s.F.Kubectl(namespace, "delete", "-f", path, "--ignore-not-found=true").Run()
	})
}

// StreamGatewayLogs opens a log stream for the named Gateway's pod and returns
// an io.ReadCloser. The stream follows logs in real-time. Callers may close
// the stream early if desired; cleanup is also registered automatically to
// stop the stream when the scenario ends.
func (s *Scenario) StreamGatewayLogs(namespace, gatewayName string) io.ReadCloser {
	s.T.Helper()
	ctx := s.T.Context()

	// Find the Gateway pod using the standard label selector.
	labelSelector := fmt.Sprintf(
		"gateway.networking.k8s.io/gateway-name=%s", gatewayName,
	)

	var podName, containerName string
	// Poll until we find a Running and Ready pod with at least one container.
	require.Eventually(
		s.T,
		func() bool {
			pods, err := s.F.KubeClient.CoreV1().Pods(namespace).List(
				ctx,
				metav1.ListOptions{LabelSelector: labelSelector},
			)
			if err != nil {
				s.T.Logf("failed to list pods for gateway %s/%s: %v", namespace, gatewayName, err)
				return false
			}
			if len(pods.Items) == 0 {
				return false
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					continue
				}
				ready := false
				for _, cond := range pod.Status.Conditions {
					if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
						ready = true
						break
					}
				}
				if !ready {
					continue
				}
				if len(pod.Spec.Containers) == 0 {
					continue
				}
				podName = pod.Name
				containerName = pod.Spec.Containers[0].Name
				return true
			}
			return false
		},
		DefaultTimeout,
		DefaultInterval,
		"no running/ready pods found for gateway %s/%s", namespace, gatewayName,
	)
	// Open the log stream with Follow enabled for real-time streaming.
	logOpts := &corev1.PodLogOptions{
		Container: containerName,
		Follow:    true,
	}
	req := s.F.KubeClient.CoreV1().Pods(namespace).GetLogs(podName, logOpts)
	stream, err := req.Stream(ctx)
	require.NoError(s.T, err, "open log stream for %s/%s", namespace, podName)

	s.T.Logf("Streaming logs from %s/%s (%s)", namespace, podName, containerName)

	// Register cleanup to close the stream.
	s.OnCleanup(func() {
		if err := stream.Close(); err != nil {
			s.T.Logf("cleanup: failed to close log stream for %s/%s: %v", namespace, podName, err)
		}
	})

	return stream
}

// -----------------------------------------------------------------------------
// Failure Diagnostics
// -----------------------------------------------------------------------------

// dumpOnFailure collects diagnostic information when the test has failed.
// It runs as a t.Cleanup function before resource cleanup (LIFO ordering).
func (s *Scenario) dumpOnFailure() {
	if !s.T.Failed() {
		return
	}

	for _, ns := range s.namespaces {
		s.dumpNamespace(ns)
	}
}

func (s *Scenario) dumpNamespace(ns string) {
	s.T.Logf("=== DIAGNOSTIC DUMP for namespace %s ===", ns)

	dumpCmds := []struct {
		label string
		args  []string
	}{
		{"engines", []string{"get", "engines.waf.k8s.coraza.io", "-o", "yaml"}},
		{"rulesets", []string{"get", "rulesets.waf.k8s.coraza.io", "-o", "yaml"}},
		{"wasmplugins", []string{"get", "wasmplugins.extensions.istio.io", "-o", "yaml"}},
		{"events", []string{"get", "events", "--sort-by=.lastTimestamp"}},
		{"pods", []string{"get", "pods", "-o", "wide"}},
	}

	for _, dc := range dumpCmds {
		out, err := s.F.Kubectl(ns, dc.args...).CombinedOutput()
		if err != nil {
			s.T.Logf("[%s] error: %v\n%s", dc.label, err, string(out))
			continue
		}
		s.T.Logf("[%s]\n%s", dc.label, string(out))
	}

	// Collect pod logs for gateway pods in this namespace.
	pods, err := s.F.KubeClient.CoreV1().Pods(ns).List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		s.T.Logf("[pod-logs] error listing pods: %v", err)
	} else {
		for _, pod := range pods.Items {
			for _, c := range pod.Spec.Containers {
				out, logErr := s.F.Kubectl(ns, "logs", pod.Name, "-c", c.Name,
					"--tail=50").CombinedOutput()
				if logErr != nil {
					s.T.Logf("[pod-logs] %s/%s: error: %v", pod.Name, c.Name, logErr)
					continue
				}
				s.T.Logf("[pod-logs] %s/%s:\n%s", pod.Name, c.Name, string(out))
			}
		}
	}

	s.writeArtifacts(ns)
}

func (s *Scenario) writeArtifacts(ns string) {
	artifactsDir := os.Getenv("ARTIFACTS_DIR")
	if artifactsDir == "" {
		return
	}

	// Sanitize test name for filesystem use.
	testName := strings.ReplaceAll(s.T.Name(), "/", "_")
	dir := filepath.Join(artifactsDir, testName, ns)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.T.Logf("artifacts: failed to create dir %s: %v", dir, err)
		return
	}

	artifacts := []struct {
		filename string
		args     []string
	}{
		{"engines.yaml", []string{"get", "engines.waf.k8s.coraza.io", "-o", "yaml"}},
		{"rulesets.yaml", []string{"get", "rulesets.waf.k8s.coraza.io", "-o", "yaml"}},
		{"wasmplugins.yaml", []string{"get", "wasmplugins.extensions.istio.io", "-o", "yaml"}},
		{"events.txt", []string{"get", "events", "--sort-by=.lastTimestamp"}},
		{"pods.txt", []string{"describe", "pods"}},
	}

	for _, a := range artifacts {
		out, err := s.F.Kubectl(ns, a.args...).CombinedOutput()
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(dir, a.filename), out, 0o644)
	}

	s.T.Logf("artifacts written to %s", dir)
}
