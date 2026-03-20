//go:build integration

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

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestRapidRuleUpdates verifies that rapidly updating ConfigMap rules
// multiple times results in the final state being correctly applied.
func TestRapidRuleUpdates(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("rapid-updates")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy initial rules")
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
	s.CreateConfigMap(ns, "block-rules", framework.SimpleBlockRule(11001, "initial"))
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")
	gw.ExpectBlocked("/?test=initial")

	s.Step("perform 10 rapid rule updates")
	for i := 1; i <= 10; i++ {
		pattern := fmt.Sprintf("pattern-%d", i)
		s.UpdateConfigMap(ns, "block-rules", framework.SimpleBlockRule(11001+i, pattern))
		// Small delay to allow some processing, but rapid enough to test race conditions
		time.Sleep(100 * time.Millisecond)
	}

	s.Step("verify final state is applied")
	// Wait a bit for reconciliation to settle
	time.Sleep(2 * time.Second)

	// Final pattern should be "pattern-10"
	gw.ExpectBlocked("/?test=pattern-10")

	// Previous patterns should no longer be blocked
	gw.ExpectAllowed("/?test=pattern-1")
	gw.ExpectAllowed("/?test=pattern-5")
	gw.ExpectAllowed("/?test=initial")
}

// TestGatewayPodRestart verifies that WAF rules re-apply after
// a Gateway pod is restarted.
func TestGatewayPodRestart(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("gw-restart")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy rules")
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
	s.CreateConfigMap(ns, "block-rules", framework.SimpleBlockRule(11201, "blocked"))
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	s.Step("verify WAF is working before restart")
	gw.ExpectBlocked("/?test=blocked")
	gw.ExpectAllowed("/?test=safe")

	s.Step("delete gateway pods to simulate restart")
	pods, err := s.F.KubeClient.CoreV1().Pods(ns).List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("gateway.networking.k8s.io/gateway-name=%s", "gw"),
		},
	)
	if err != nil {
		t.Fatalf("failed to list gateway pods: %v", err)
	}

	for _, pod := range pods.Items {
		err := s.F.KubeClient.CoreV1().Pods(ns).Delete(
			context.Background(), pod.Name, metav1.DeleteOptions{},
		)
		if err != nil {
			t.Logf("failed to delete pod %s: %v", pod.Name, err)
		}
	}

	s.Step("wait for gateway to be ready again")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("verify WAF rules re-apply after restart")
	// The proxy will reconnect to the new pod
	gw.ExpectBlocked("/?test=blocked")
	gw.ExpectAllowed("/?test=safe")
}

// TestEngineRecreateAfterGateway verifies that creating a Gateway after
// the Engine exists properly applies WAF rules.
func TestEngineRecreateAfterGateway(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("engine-first")

	s.Step("deploy rules")
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
	s.CreateConfigMap(ns, "block-rules", framework.SimpleBlockRule(11301, "blocked"))
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

	s.Step("create engine before gateway exists")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")
	s.ExpectEngineGateways(ns, "engine", nil) // No gateways yet

	s.Step("create gateway after engine")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("verify engine discovers the gateway")
	s.ExpectEngineGateways(ns, "engine", []string{"gw"})

	s.Step("deploy backend")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	s.Step("verify WAF is enforced")
	gw := s.ProxyToGateway(ns, "gw")
	gw.ExpectBlocked("/?test=blocked")
	gw.ExpectAllowed("/?test=safe")
}

// TestConcurrentRuleSetUpdates verifies that updating multiple RuleSets
// concurrently doesn't cause issues.
func TestConcurrentRuleSetUpdates(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("concurrent")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	// Create 3 independent engines with their own rulesets
	engines := []struct {
		name    string
		pattern string
		ruleID  int
	}{
		{"engine-1", "alpha", 11401},
		{"engine-2", "beta", 11402},
		{"engine-3", "gamma", 11403},
	}

	s.Step("deploy multiple engines with separate rulesets")
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)

	for _, e := range engines {
		s.CreateConfigMap(ns, fmt.Sprintf("rules-%s", e.name), framework.SimpleBlockRule(e.ruleID, e.pattern))
		s.CreateRuleSet(ns, fmt.Sprintf("ruleset-%s", e.name), []string{"base-rules", fmt.Sprintf("rules-%s", e.name)})
		s.CreateEngine(ns, e.name, framework.EngineOpts{
			RuleSetName: fmt.Sprintf("ruleset-%s", e.name),
			GatewayName: "gw",
		})
		s.ExpectEngineReady(ns, e.name)
	}

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	s.Step("verify initial state")
	gw.ExpectBlocked("/?test=alpha")
	gw.ExpectBlocked("/?test=beta")
	gw.ExpectBlocked("/?test=gamma")

	s.Step("update all rulesets concurrently")
	// Update all ConfigMaps rapidly
	for i, e := range engines {
		newPattern := fmt.Sprintf("updated-%d", i+1)
		s.UpdateConfigMap(ns, fmt.Sprintf("rules-%s", e.name), framework.SimpleBlockRule(e.ruleID+100, newPattern))
	}

	s.Step("wait for reconciliation")
	time.Sleep(3 * time.Second)

	s.Step("verify all updates applied correctly")
	gw.ExpectBlocked("/?test=updated-1")
	gw.ExpectBlocked("/?test=updated-2")
	gw.ExpectBlocked("/?test=updated-3")

	// Old patterns should no longer be blocked
	gw.ExpectAllowed("/?test=alpha")
	gw.ExpectAllowed("/?test=beta")
	gw.ExpectAllowed("/?test=gamma")
}
