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

// TestEngineGatewayStatus validates that the Engine's status.gateways field
// is populated with the Gateways that match the Engine's workload selector.
func TestEngineGatewayStatus(t *testing.T) {
	t.Parallel()

	// -------------------------------------------------------------------------
	// Sub-test: No matching Gateways
	// -------------------------------------------------------------------------

	t.Run("no_matching_gateways", func(t *testing.T) {
		t.Parallel()
		s := fw.NewScenario(t)

		ns := s.GenerateNamespace("gw-status-0")

		s.Step("create rules and engine targeting non-existent gateway")
		s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
		s.CreateRuleSet(ns, "ruleset", []string{"base-rules"})
		s.CreateEngine(ns, "engine", framework.EngineOpts{
			RuleSetName: "ruleset",
			GatewayName: "nonexistent-gateway",
		})

		s.Step("verify engine is ready with no gateways in status")
		s.ExpectEngineReady(ns, "engine")
		s.ExpectEngineGateways(ns, "engine", nil)
	})

	// -------------------------------------------------------------------------
	// Sub-test: One matching Gateway
	// -------------------------------------------------------------------------

	t.Run("one_matching_gateway", func(t *testing.T) {
		t.Parallel()
		s := fw.NewScenario(t)

		ns := s.GenerateNamespace("gw-status-1")

		s.Step("create rules")
		s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
		s.CreateConfigMap(ns, "block-rules",
			framework.SimpleBlockRule(4001, "blocked"),
		)
		s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

		s.Step("create gateway and engine")
		s.CreateGateway(ns, "gw")
		s.ExpectGatewayProgrammed(ns, "gw")

		s.CreateEngine(ns, "engine", framework.EngineOpts{
			RuleSetName: "ruleset",
			GatewayName: "gw",
		})

		s.Step("verify engine has one gateway in status")
		s.ExpectEngineReady(ns, "engine")
		s.ExpectEngineGateways(ns, "engine", []string{"gw"})

		s.Step("verify WAF enforcement")
		s.CreateEchoBackend(ns, "echo")
		s.CreateHTTPRoute(ns, "echo-route", "gw", "echo")
		gw := s.ProxyToGateway(ns, "gw")
		gw.ExpectBlocked("/?test=blocked")
		gw.ExpectAllowed("/?test=safe")
	})

	// -------------------------------------------------------------------------
	// Sub-test: Three matching Gateways
	// -------------------------------------------------------------------------

	t.Run("three_matching_gateways", func(t *testing.T) {
		t.Parallel()
		s := fw.NewScenario(t)

		ns := s.GenerateNamespace("gw-status-3")
		gwCount := 3

		s.Step("create rules")
		s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
		s.CreateConfigMap(ns, "block-rules",
			framework.SimpleBlockRule(4002, "blocked"),
		)
		s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

		s.Step("create gateways")
		gwNames := make([]string, gwCount)
		for i := range gwCount {
			gwNames[i] = fmt.Sprintf("gw-%d", i+1)
			s.CreateGateway(ns, gwNames[i])
			s.ExpectGatewayProgrammed(ns, gwNames[i])
		}

		s.Step("create engine targeting all gateways")
		s.CreateEngine(ns, "engine", framework.EngineOpts{
			RuleSetName:  "ruleset",
			GatewayNames: gwNames,
		})

		s.Step("verify engine has all gateways in status")
		s.ExpectEngineReady(ns, "engine")
		s.ExpectEngineGateways(ns, "engine", gwNames)

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
