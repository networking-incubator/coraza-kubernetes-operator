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

// TestMultipleGateways validates that RuleSets can be deployed to 3+
// Gateways simultaneously and that each Gateway independently enforces
// the WAF rules.
//
// Related: https://github.com/networking-incubator/coraza-kubernetes-operator/issues/13
func TestMultipleGateways(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("multi-gw")
	gatewayCount := 3

	// -------------------------------------------------------------------------
	// Step 1: Create shared rule set
	// -------------------------------------------------------------------------

	s.Step("create shared rules")

	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
	s.CreateRuleSource(ns, "block-rules",
		framework.SimpleBlockRule(1001, "blocked"),
	)

	s.CreateRuleSet(ns, "shared-ruleset", []string{"base-rules", "block-rules"})

	// -------------------------------------------------------------------------
	// Step 2: Create gateways and engines
	// -------------------------------------------------------------------------

	s.Step("deploy shared echo backend")
	s.CreateEchoBackend(ns, "echo")

	s.Step("create gateways and engines")

	proxies := make([]*framework.GatewayProxy, gatewayCount)
	for i := 1; i <= gatewayCount; i++ {
		gwName := fmt.Sprintf("gateway-%d", i)
		engineName := fmt.Sprintf("engine-%d", i)
		routeName := fmt.Sprintf("echo-route-%d", i)

		s.CreateGateway(ns, gwName)
		s.ExpectGatewayProgrammed(ns, gwName)

		s.CreateEngine(ns, engineName, framework.EngineOpts{
			RuleSetName: "shared-ruleset",
			GatewayName: gwName,
		})
		s.ExpectEngineReady(ns, engineName)

		s.CreateHTTPRoute(ns, routeName, gwName, "echo")
		proxies[i-1] = s.ProxyToGateway(ns, gwName)
	}

	// -------------------------------------------------------------------------
	// Step 3: Verify all gateways block malicious traffic
	// -------------------------------------------------------------------------

	s.Step("verify all gateways block malicious traffic")

	for i, gw := range proxies {
		t.Logf("Testing gateway-%d blocks malicious request", i+1)
		gw.ExpectBlocked("/?test=blocked")
	}

	// -------------------------------------------------------------------------
	// Step 4: Verify all gateways allow clean traffic
	// -------------------------------------------------------------------------

	s.Step("verify all gateways allow clean traffic")

	for i, gw := range proxies {
		t.Logf("Testing gateway-%d allows clean request", i+1)
		gw.ExpectAllowed("/?test=safe")
	}
}
