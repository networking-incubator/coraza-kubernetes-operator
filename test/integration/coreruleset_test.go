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
	"net/http"
	"testing"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestCoreRulesetCompatibility validates that CoreRuleSet-style rules can be
// loaded and enforced through the operator. This uses a representative subset
// of CRS rules (SQLi, XSS detection) rather than the full CoreRuleSet to
// keep the test focused.
//
// For full CRS loading, consider importing github.com/corazawaf/coraza-coreruleset
// and loading the complete ruleset into ConfigMaps.
//
// Related: https://github.com/networking-incubator/coraza-kubernetes-operator/issues/12
func TestCoreRulesetCompatibility(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("crs-compat")

	// -------------------------------------------------------------------------
	// Step 1: Set up a Gateway for this test
	// -------------------------------------------------------------------------

	s.Step("create gateway")
	s.CreateGateway(ns, "crs-gateway")
	s.ExpectGatewayProgrammed(ns, "crs-gateway")

	// -------------------------------------------------------------------------
	// Step 2: Deploy CRS-compatible rules
	// -------------------------------------------------------------------------

	s.Step("deploy coreruleset-compatible rules")

	s.CreateConfigMap(ns, "base-rules", `
SecRuleEngine On
SecRequestBodyAccess On
SecResponseBodyAccess Off
SecAuditLog /dev/stdout
SecAuditLogFormat JSON
SecAuditEngine RelevantOnly
`)

	// SQL injection detection (inspired by CRS rule 942100)
	s.CreateConfigMap(ns, "sqli-rules", `
SecRule ARGS "@rx (?i:(\b(select|union|insert|update|delete|drop)\b.*\b(from|into|where|table)\b))" \
  "id:942100,\
  phase:2,\
  deny,\
  status:403,\
  t:none,t:urlDecodeUni,\
  msg:'SQL Injection Attack Detected',\
  severity:'CRITICAL'"
`)

	// XSS detection (inspired by CRS rule 941100)
	s.CreateConfigMap(ns, "xss-rules", `
SecRule ARGS "@rx (?i:<script[^>]*>)" \
  "id:941100,\
  phase:2,\
  deny,\
  status:403,\
  t:none,t:urlDecodeUni,t:htmlEntityDecode,\
  msg:'XSS Attack Detected',\
  severity:'CRITICAL'"
`)

	s.CreateRuleSet(ns, "crs-ruleset", []string{"base-rules", "sqli-rules", "xss-rules"})

	// -------------------------------------------------------------------------
	// Step 3: Create Engine targeting the gateway
	// -------------------------------------------------------------------------

	s.Step("create engine")
	s.CreateEngine(ns, "crs-engine", framework.EngineOpts{
		RuleSetName: "crs-ruleset",
		GatewayName: "crs-gateway",
	})

	s.Step("wait for engine ready")
	s.ExpectEngineReady(ns, "crs-engine")
	s.ExpectWasmPluginExists(ns, "coraza-engine-crs-engine")

	s.Step("verify operator emitted expected events")
	s.ExpectEvent(ns, framework.EventMatch{Type: "Normal", Reason: "RulesCached"})
	s.ExpectEvent(ns, framework.EventMatch{Type: "Normal", Reason: "WasmPluginCreated"})

	// -------------------------------------------------------------------------
	// Step 4: Deploy backend and verify WAF enforcement
	// -------------------------------------------------------------------------

	s.Step("deploy echo backend")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "echo-route", "crs-gateway", "echo")

	gw := s.ProxyToGateway(ns, "crs-gateway")

	s.Step("verify SQL injection is blocked")
	gw.ExpectStatus("/?id=1+UNION+SELECT+username+FROM+users", http.StatusForbidden)

	s.Step("verify XSS is blocked")
	gw.ExpectStatus("/?p=<script>alert(1)</script>", http.StatusForbidden)

	s.Step("verify normal traffic passes through to backend")
	gw.ExpectAllowed("/?q=hello+world")
}
