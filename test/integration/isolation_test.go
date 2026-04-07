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
	"testing"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestCrossNamespaceIsolation verifies that a RuleSet in namespace A
// cannot affect a Gateway in namespace B - each namespace is isolated.
func TestCrossNamespaceIsolation(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	nsA := s.GenerateNamespace("isolation-a")
	nsB := s.GenerateNamespace("isolation-b")

	// --- Namespace A: Gateway with strict blocking rules ---

	s.Step("create gateway in namespace A")
	s.CreateGateway(nsA, "gw")
	s.ExpectGatewayProgrammed(nsA, "gw")

	s.Step("deploy strict blocking rules in namespace A")
	s.CreateRuleSource(nsA, "base-rules", `SecRuleEngine On`)
	s.CreateRuleSource(nsA, "strict-rules", framework.SimpleBlockRule(10001, "blocked"))
	s.CreateRuleSet(nsA, "ruleset", []string{"base-rules", "strict-rules"})

	s.CreateEngine(nsA, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(nsA, "engine")

	s.CreateEchoBackend(nsA, "echo")
	s.CreateHTTPRoute(nsA, "route", "gw", "echo")

	// --- Namespace B: Gateway with permissive rules ---

	s.Step("create gateway in namespace B")
	s.CreateGateway(nsB, "gw")
	s.ExpectGatewayProgrammed(nsB, "gw")

	s.Step("deploy permissive rules in namespace B")
	s.CreateRuleSource(nsB, "base-rules", `SecRuleEngine On`)
	// Different rule - only blocks "different-pattern"
	s.CreateRuleSource(nsB, "permissive-rules", framework.SimpleBlockRule(10002, "different-pattern"))
	s.CreateRuleSet(nsB, "ruleset", []string{"base-rules", "permissive-rules"})

	s.CreateEngine(nsB, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(nsB, "engine")

	s.CreateEchoBackend(nsB, "echo")
	s.CreateHTTPRoute(nsB, "route", "gw", "echo")

	// --- Verify isolation ---

	s.Step("verify namespace A blocks 'blocked' pattern")
	gwA := s.ProxyToGateway(nsA, "gw")
	gwA.ExpectBlocked("/?test=blocked")
	gwA.ExpectAllowed("/?test=different-pattern") // A's rules don't block this

	s.Step("verify namespace B has different rules")
	gwB := s.ProxyToGateway(nsB, "gw")
	gwB.ExpectAllowed("/?test=blocked")           // B's rules don't block this
	gwB.ExpectBlocked("/?test=different-pattern") // B blocks this instead
}
