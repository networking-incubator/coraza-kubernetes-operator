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
	"fmt"
	"testing"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestEngineGatewayTarget validates that an Engine correctly targets Gateway
// resources via target and enforces WAF rules on their traffic.
func TestEngineGatewayTarget(t *testing.T) {
	t.Parallel()

	// -------------------------------------------------------------------------
	// Sub-test: Engine targeting non-existent Gateway still becomes Ready
	// -------------------------------------------------------------------------

	t.Run("nonexistent_gateway", func(t *testing.T) {
		t.Parallel()
		s := fw.NewScenario(t)

		ns := s.GenerateNamespace("gw-target-0")

		s.Step("create rules and engine targeting non-existent gateway")
		s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
		s.CreateRuleSet(ns, "ruleset", []string{"base-rules"}, nil)
		s.CreateEngine(ns, "engine", framework.EngineOpts{
			RuleSetName: "ruleset",
			GatewayName: "nonexistent-gateway",
		})

		s.Step("verify engine is ready")
		s.ExpectEngineReady(ns, "engine")
	})

	// -------------------------------------------------------------------------
	// Sub-test: Single Gateway with WAF enforcement
	// -------------------------------------------------------------------------

	t.Run("single_gateway", func(t *testing.T) {
		t.Parallel()
		s := fw.NewScenario(t)

		ns := s.GenerateNamespace("gw-target-1")

		s.Step("create rules")
		s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
		s.CreateRuleSource(ns, "block-rules",
			framework.SimpleBlockRule(4001, "blocked"),
		)
		s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"}, nil)

		s.Step("create gateway and engine")
		s.CreateGateway(ns, "gw")
		s.ExpectGatewayProgrammed(ns, "gw")

		s.CreateEngine(ns, "engine", framework.EngineOpts{
			RuleSetName: "ruleset",
			GatewayName: "gw",
		})

		s.Step("verify engine is ready")
		s.ExpectEngineReady(ns, "engine")

		s.Step("verify WAF enforcement")
		s.CreateEchoBackend(ns, "echo")
		s.CreateHTTPRoute(ns, "echo-route", "gw", "echo")
		gw := s.ProxyToGateway(ns, "gw")
		gw.ExpectBlocked("/?test=blocked")
		gw.ExpectAllowed("/?test=safe")
	})

	// -------------------------------------------------------------------------
	// Sub-test: Three Gateways, one Engine each
	// -------------------------------------------------------------------------

	t.Run("three_gateways_one_engine_each", func(t *testing.T) {
		t.Parallel()
		s := fw.NewScenario(t)

		ns := s.GenerateNamespace("gw-target-3")
		gwCount := 3

		s.Step("create rules")
		s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
		s.CreateRuleSource(ns, "block-rules",
			framework.SimpleBlockRule(4002, "blocked"),
		)
		s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"}, nil)

		s.Step("create gateways and engines")
		gwNames := make([]string, gwCount)
		for i := range gwCount {
			gwNames[i] = fmt.Sprintf("gw-%d", i+1)
			s.CreateGateway(ns, gwNames[i])
			s.ExpectGatewayProgrammed(ns, gwNames[i])
			s.CreateEngine(ns, fmt.Sprintf("engine-%d", i+1), framework.EngineOpts{
				RuleSetName: "ruleset",
				GatewayName: gwNames[i],
			})
		}

		s.Step("verify each engine is ready")
		for i := range gwNames {
			s.ExpectEngineReady(ns, fmt.Sprintf("engine-%d", i+1))
		}

		s.Step("deploy echo backend and verify WAF on all gateways")
		s.CreateEchoBackend(ns, "echo")

		for _, gwName := range gwNames {
			s.CreateHTTPRoute(ns, fmt.Sprintf("route-%s", gwName), gwName, "echo")
			gw := s.ProxyToGateway(ns, gwName)
			t.Logf("Testing gateway %s blocks malicious request", gwName)
			gw.ExpectBlocked("/?test=blocked")
			t.Logf("Testing gateway %s allows clean request", gwName)
			gw.ExpectAllowed("/?test=safe")
		}
	})
}
