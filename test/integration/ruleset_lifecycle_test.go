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
	"net/url"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestRuleSetDeletion verifies graceful handling when a RuleSet is deleted
// while an Engine still references it.
func TestRuleSetDeletion(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("ruleset-delete")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy initial rules")
	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
	s.CreateRuleSource(ns, "block-rules", framework.SimpleBlockRule(9001, "blocked"))
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})
	s.ExpectRuleSetReady(ns, "ruleset")

	s.Step("create engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("deploy backend and verify WAF works")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")
	gw.ExpectBlocked("/?test=blocked")
	gw.ExpectAllowed("/?test=safe")

	s.Step("delete ruleset")
	err := s.F.DynamicClient.Resource(framework.RuleSetGVR).Namespace(ns).Delete(
		context.Background(), "ruleset", metav1.DeleteOptions{},
	)
	if err != nil {
		t.Fatalf("failed to delete ruleset: %v", err)
	}

	s.Step("verify cached rules still apply after ruleset deletion")
	// The WAF should continue using cached rules
	gw.ExpectBlocked("/?test=blocked")
	gw.ExpectAllowed("/?test=safe")
}

// TestEngineDeleteRecreate verifies that deleting and recreating an Engine
// properly cleans up and recreates the WasmPlugin.
func TestEngineDeleteRecreate(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("engine-recreate")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy rules")
	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
	s.CreateRuleSource(ns, "block-rules", framework.SimpleBlockRule(9101, "firstblock"))
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

	s.Step("create first engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")
	s.ExpectWasmPluginExists(ns, "coraza-engine-engine")

	s.Step("deploy backend")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")
	gw.ExpectBlocked("/?test=firstblock")

	s.Step("delete engine")
	err := s.F.DynamicClient.Resource(framework.EngineGVR).Namespace(ns).Delete(
		context.Background(), "engine", metav1.DeleteOptions{},
	)
	if err != nil {
		t.Fatalf("failed to delete engine: %v", err)
	}

	s.Step("verify WasmPlugin is cleaned up")
	s.ExpectResourceGone(ns, "coraza-engine-engine", framework.WasmPluginGVR)

	s.Step("update rules for second engine")
	s.UpdateRuleSource(ns, "block-rules", framework.SimpleBlockRule(9102, "secondblock"))

	s.Step("recreate engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")
	s.ExpectWasmPluginExists(ns, "coraza-engine-engine")

	s.Step("verify new rules are applied")
	gw.ExpectBlocked("/?test=secondblock")
	gw.ExpectAllowed("/?test=firstblock") // Old rule should no longer block
}

// TestRuleSourceDeletion verifies that deleting a RuleSource referenced by a RuleSet
// causes the RuleSet to become degraded, but cached rules continue to apply.
func TestRuleSourceDeletion(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("rs-delete")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy rules")
	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
	s.CreateRuleSource(ns, "block-rules", framework.SimpleBlockRule(9201, "blocked"))
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})
	s.ExpectRuleSetReady(ns, "ruleset")

	s.Step("create engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("deploy backend and verify WAF works")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")
	gw.ExpectBlocked("/?test=blocked")

	s.Step("delete RuleSource")
	s.DeleteRuleSource(ns, "block-rules")

	s.Step("verify RuleSet becomes degraded after source deletion")
	s.ExpectRuleSetDegraded(ns, "ruleset")

	s.Step("verify cached rules still apply despite degraded RuleSet")
	gw.ExpectBlocked("/?test=blocked")
	gw.ExpectAllowed("/?test=safe")
}

// TestRuleSetOrderMatters verifies that RuleSources in a RuleSet are processed
// in order, and rule precedence is maintained.
func TestRuleSetOrderMatters(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("rule-order")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy rules with specific order")
	// First rule: allow everything with "safe" prefix
	s.CreateRuleSource(ns, "allow-rules", `SecRuleEngine On
SecRule REQUEST_URI "@beginsWith /safe" "id:9301,phase:1,pass,nolog"`)

	// Second rule: block everything with "blocked" (should apply after allow)
	s.CreateRuleSource(ns, "block-rules", `SecRule REQUEST_URI|ARGS "@contains blocked" "id:9302,phase:2,deny,status:403"`)

	// Third rule: specific override - block even safe paths with "override"
	s.CreateRuleSource(ns, "override-rules", `SecRule REQUEST_URI "@contains override" "id:9303,phase:1,deny,status:403"`)

	s.CreateRuleSet(ns, "ruleset", []string{"allow-rules", "block-rules", "override-rules"})

	s.Step("create engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("deploy backend")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	s.Step("verify rule order is respected")
	gw.ExpectAllowed("/safe/path")
	gw.ExpectBlocked("/?test=blocked")
	gw.ExpectBlocked("/safe/override") // Override rule should block even safe paths
}

// TestEmptyRuleSet verifies that a RuleSet with SecRuleEngine Off
// allows all traffic to pass through.
func TestEmptyRuleSet(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("empty-ruleset")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy rules with engine disabled")
	s.CreateRuleSource(ns, "disabled-rules", `SecRuleEngine Off`)
	s.CreateRuleSet(ns, "ruleset", []string{"disabled-rules"})

	s.Step("create engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("deploy backend")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	s.Step("verify all traffic passes through")
	gw.ExpectAllowed("/")
	gw.ExpectAllowed("/?attack=" + url.QueryEscape("<script>alert(1)</script>"))
	gw.ExpectAllowed("/?sql=" + url.QueryEscape("DROP TABLE users"))
	gw.ExpectAllowed("/admin/../../../etc/passwd")
}
