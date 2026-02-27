//go:build integration

/*
Copyright 2026 Shane Utt.

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

// TestReconciliation validates that the operator's reconciliation loop
// reacts to live resource changes (RuleSet mutations, ConfigMap content
// updates) and propagates them to the WAF.
func TestReconciliation(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("reconcile")

	// --- deploy initial rules and engine ---

	s.Step("create gateway")
	s.CreateGateway(ns, "reconcile-gw")
	s.ExpectGatewayProgrammed(ns, "reconcile-gw")

	s.Step("deploy initial rules")
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
	s.CreateConfigMap(ns, "block-evil",
		framework.SimpleBlockRule(3001, "evilmonkey"),
	)
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-evil"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "reconcile-gw",
	})
	s.ExpectEngineReady(ns, "engine")
	s.ExpectWasmPluginExists(ns, "coraza-engine-engine")

	s.Step("verify operator emitted expected events")
	s.ExpectEvent(ns, framework.EventMatch{Type: "Normal", Reason: "RulesCached"})
	s.ExpectEvent(ns, framework.EventMatch{Type: "Normal", Reason: "WasmPluginCreated"})

	s.Step("deploy echo backend")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "echo-route", "reconcile-gw", "echo")

	gw := s.ProxyToGateway(ns, "reconcile-gw")

	s.Step("verify initial rules enforce")
	gw.ExpectBlocked("/?test=evilmonkey")
	gw.ExpectAllowed("/?test=safe")

	// --- RuleSet mutation: add a new ConfigMap ref ---

	s.Step("add sinistermonkey rule to ruleset")
	s.CreateConfigMap(ns, "block-sinister",
		framework.SimpleBlockRule(3002, "sinistermonkey"),
	)
	s.UpdateRuleSet(ns, "ruleset", []string{"base-rules", "block-evil", "block-sinister"})

	gw.ExpectBlocked("/sinistermonkey")

	// --- ConfigMap content update: replace rule in-place ---

	s.Step("replace sinistermonkey rule with maniacalmonkey")
	s.UpdateConfigMap(ns, "block-sinister",
		framework.SimpleBlockRule(3002, "maniacalmonkey"),
	)

	gw.ExpectAllowed("/sinistermonkey")
	gw.ExpectBlocked("/maniacalmonkey")
}
