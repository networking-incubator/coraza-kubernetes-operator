//go:build e2e

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

package e2e

import (
	"os"
	"testing"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestE2E_BasicTraffic validates that the entire operator stack running against a real
// Istio Gateway deployment successfully intercepts and enforces WAF rules on data-plane traffic.
func TestE2E_BasicTraffic(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("e2e-basic-traffic")

	// -------------------------------------------------------------------------
	// Step 1: Deploy a Gateway utilizing the specified GatewayClass
	// -------------------------------------------------------------------------

	gatewayClass := os.Getenv("GATEWAY_CLASS")
	if gatewayClass == "" {
		gatewayClass = "istio"
	}

	s.Step("create gateway")
	s.CreateGatewayWithClass(ns, "istio-gateway", gatewayClass)
	s.ExpectGatewayProgrammed(ns, "istio-gateway")

	// -------------------------------------------------------------------------
	// Step 2: Deploy Coraza rules
	// -------------------------------------------------------------------------

	s.Step("deploy coraza rules")

	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
	s.CreateRuleSource(ns, "block-rules",
		framework.SimpleBlockRule(1234, "blocked"),
	)

	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

	// -------------------------------------------------------------------------
	// Step 3: Create Engine targeting the gateway
	// -------------------------------------------------------------------------

	s.Step("create engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "istio-gateway",
	})

	s.Step("wait for engine ready")
	s.ExpectEngineReady(ns, "engine")
	s.ExpectWasmPluginExists(ns, "coraza-engine-engine")

	// -------------------------------------------------------------------------
	// Step 4: Deploy backend and verify WAF enforcement
	// -------------------------------------------------------------------------

	s.Step("deploy echo backend")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "echo-route", "istio-gateway", "echo")

	// -------------------------------------------------------------------------
	// Step 5: Route traffic and verify protection
	// -------------------------------------------------------------------------

	s.Step("setup proxy to gateway")
	gw := s.ProxyToGateway(ns, "istio-gateway")

	s.Step("verify malicious traffic is blocked")
	gw.ExpectBlocked("/?test=blocked")

	s.Step("verify clean traffic is allowed")
	gw.ExpectAllowed("/?test=safe")
}
