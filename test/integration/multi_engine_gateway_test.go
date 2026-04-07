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

// TestMultiEngineMultiGateway validates the behavior of multi-engine and
// multi-gateway combinations:
//
//   - An Engine targeting multiple Gateways (via separate engines per gateway,
//     or a shared label selector when supported).
//   - Multiple Engines attempting to attach to a single Gateway.
//   - An Engine whose label selector matches no Gateways.
//
// Related: https://github.com/networking-incubator/coraza-kubernetes-operator/issues/52
func TestMultiEngineMultiGateway(t *testing.T) {
	t.Parallel()

	// -------------------------------------------------------------------------
	// Sub-test: One Engine per Gateway, multiple Gateways sharing a RuleSet
	// -------------------------------------------------------------------------

	t.Run("engine_per_gateway_shared_ruleset", func(t *testing.T) {
		t.Parallel()
		s := fw.NewScenario(t)

		ns := s.GenerateNamespace("multi-target")

		s.Step("create shared rules")
		s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
		s.CreateRuleSource(ns, "block-rules",
			framework.SimpleBlockRule(1001, "evil"),
		)
		s.CreateRuleSet(ns, "shared-rules", []string{"base-rules", "block-rules"})

		s.Step("deploy shared echo backend")
		s.CreateEchoBackend(ns, "echo")

		s.Step("create gateways and engines")
		for i := 1; i <= 2; i++ {
			gwName := fmt.Sprintf("shared-gw-%d", i)
			engineName := fmt.Sprintf("engine-%d", i)
			routeName := fmt.Sprintf("echo-route-%d", i)

			s.CreateGateway(ns, gwName)
			s.ExpectGatewayProgrammed(ns, gwName)

			s.CreateEngine(ns, engineName, framework.EngineOpts{
				RuleSetName: "shared-rules",
				GatewayName: gwName,
			})
			s.ExpectEngineReady(ns, engineName)

			s.CreateHTTPRoute(ns, routeName, gwName, "echo")
		}

		s.Step("verify both gateways enforce rules")
		for i := 1; i <= 2; i++ {
			gwName := fmt.Sprintf("shared-gw-%d", i)
			gw := s.ProxyToGateway(ns, gwName)
			gw.ExpectBlocked("/?test=evil")
			gw.ExpectAllowed("/?test=safe")
		}
	})

	// -------------------------------------------------------------------------
	// Sub-test: Multiple Engines targeting the same Gateway
	// -------------------------------------------------------------------------

	t.Run("multiple_engines_single_gateway", func(t *testing.T) {
		t.Parallel()
		s := fw.NewScenario(t)

		ns := s.GenerateNamespace("multi-engine")

		s.Step("create a single gateway")
		s.CreateGateway(ns, "target-gw")
		s.ExpectGatewayProgrammed(ns, "target-gw")

		s.Step("create two different rule sets")
		s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
		s.CreateRuleSource(ns, "rules-a",
			framework.SimpleBlockRule(2001, "attackA"),
		)
		s.CreateRuleSource(ns, "rules-b",
			framework.SimpleBlockRule(2002, "attackB"),
		)
		s.CreateRuleSet(ns, "ruleset-a", []string{"base-rules", "rules-a"})
		s.CreateRuleSet(ns, "ruleset-b", []string{"base-rules", "rules-b"})

		s.Step("deploy echo backend")
		s.CreateEchoBackend(ns, "echo")
		s.CreateHTTPRoute(ns, "echo-route", "target-gw", "echo")

		s.Step("attach both engines to the gateway")
		// The operator currently accepts multiple engines targeting the
		// same gateway — there is no admission webhook preventing it.
		// Each engine creates its own WasmPlugin, and both enforce rules.
		// If issue #52 adds rejection logic, this test should be updated
		// to use TryCreateEngine and assert the error.
		s.CreateEngine(ns, "engine-a", framework.EngineOpts{
			RuleSetName: "ruleset-a",
			GatewayName: "target-gw",
		})
		s.ExpectEngineReady(ns, "engine-a")

		s.CreateEngine(ns, "engine-b", framework.EngineOpts{
			RuleSetName: "ruleset-b",
			GatewayName: "target-gw",
		})
		s.ExpectEngineReady(ns, "engine-b")

		s.Step("verify both engines enforce their rules")
		gw := s.ProxyToGateway(ns, "target-gw")
		gw.ExpectBlocked("/?test=attackA")
		gw.ExpectBlocked("/?test=attackB")
		gw.ExpectAllowed("/?test=safe")
	})

	// -------------------------------------------------------------------------
	// Sub-test: Engine targeting a non-existent Gateway
	// -------------------------------------------------------------------------

	t.Run("engine_no_matching_gateway", func(t *testing.T) {
		t.Parallel()
		s := fw.NewScenario(t)

		ns := s.GenerateNamespace("no-target")

		s.Step("create rules and engine targeting non-existent gateway")
		s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
		s.CreateRuleSet(ns, "ruleset", []string{"base-rules"})
		s.CreateEngine(ns, "orphan-engine", framework.EngineOpts{
			RuleSetName: "ruleset",
			GatewayName: "nonexistent-gateway",
		})

		s.Step("verify engine status")
		// The WasmPlugin is still created (targeting pods that don't exist).
		// Per issue #52, the engine status should reflect the degraded state
		// when Gateway watches are implemented. For now we verify the engine
		// still reaches Ready since the operator doesn't currently check
		// whether the label selector matches any pods.
		s.ExpectEngineReady(ns, "orphan-engine")
		s.ExpectWasmPluginExists(ns, "coraza-engine-orphan-engine")
	})
}
