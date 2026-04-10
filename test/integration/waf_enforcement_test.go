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
	"net/http"
	"testing"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestBlockByStatusCode verifies that different HTTP status codes (403, 406, 503)
// can be configured for blocked requests via SecLang rules.
func TestBlockByStatusCode(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		statusCode int
		ruleID     int
		target     string
	}{
		{"status_403", http.StatusForbidden, 5001, "block403"},
		{"status_406", http.StatusNotAcceptable, 5002, "block406"},
		{"status_503", http.StatusServiceUnavailable, 5003, "block503"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := fw.NewScenario(t)

			ns := s.GenerateNamespace("status-code")

			s.Step("create gateway")
			s.CreateGateway(ns, "gw")
			s.ExpectGatewayProgrammed(ns, "gw")

			s.Step("deploy rules with custom status code")
			s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
			s.CreateConfigMap(ns, "status-rules", fmt.Sprintf(
				`SecRule ARGS|REQUEST_URI "@contains %s" "id:%d,phase:2,deny,status:%d,msg:'Custom status'"`,
				tc.target, tc.ruleID, tc.statusCode,
			))
			s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "status-rules"})

			s.Step("create engine")
			s.CreateEngine(ns, "engine", framework.EngineOpts{
				RuleSetName: "ruleset",
				GatewayName: "gw",
			})
			s.ExpectEngineReady(ns, "engine")

			s.Step("deploy backend and route")
			s.CreateEchoBackend(ns, "echo")
			s.CreateHTTPRoute(ns, "route", "gw", "echo")

			s.Step("verify custom status code")
			gw := s.ProxyToGateway(ns, "gw")
			gw.ExpectStatus(fmt.Sprintf("/?test=%s", tc.target), tc.statusCode)

			s.Step("verify clean traffic allowed")
			gw.ExpectAllowed("/?test=safe")
		})
	}
}

// TestDegradedEngineDoesNotBlockTraffic verifies that an Engine referencing a
// non-existent RuleSet becomes Degraded (no WasmPlugin is created) and traffic
// continues to flow through the Gateway without being blocked when failure
// mode is set to "allow".
func TestDegradedEngineDoesNotBlockTraffic(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("degraded-engine")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("create engine referencing non-existent ruleset")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName:   "nonexistent-ruleset",
		GatewayName:   "gw",
		FailurePolicy: wafv1alpha1.FailurePolicyAllow,
	})

	s.Step("verify engine is degraded due to missing ruleset")
	s.ExpectEngineDegraded(ns, "engine")

	s.Step("deploy backend and route")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	s.Step("verify traffic flows through gateway without WAF")
	gw := s.ProxyToGateway(ns, "gw")
	gw.ExpectAllowed("/")
	gw.ExpectAllowed("/?potentially=malicious")
}

// TestRequestBodyInspection verifies that POST/PUT body content triggers
// WAF rules (SQL injection, XSS patterns in body).
func TestRequestBodyInspection(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("body-inspection")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy rules for body inspection")
	/* Some explanation about the rules below:
	We add a pre-rule setting processor for application/json (rule 200001) or defaulting to URLENCODE.
	This way, users passing bad content-type will still be parsed correctly
	*/
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On
SecDebugLogLevel 9
SecDebugLog /dev/stdout
SecRequestBodyAccess On
SecRequestBodyLimit 13107200
SecRequestBodyNoFilesLimit 131072
SecRule REQUEST_HEADERS:Content-Type "^application/json" \
    "id:200001,phase:1,t:none,t:lowercase,pass,nolog,ctl:requestBodyProcessor=JSON"
SecRule REQUEST_HEADERS:Content-Type "!@rx ^application/json" \
    "id:200000,phase:1,t:none,t:lowercase,pass,nolog,ctl:requestBodyProcessor=URLENCODED"
`)

	/*
		When Coraza successfully parses a request (like a JSON body or a standard form),
		it "explodes" the data into the ARGS collection.

		When it cannot parse the data it will set it as part of REQUEST_BODY.
		Sometimes, to save memory, a WAF might "empty" the raw REQUEST_BODY buffer
		once it has successfully moved everything into ARGS.
		If we only check REQUEST_BODY, or only ARGS we may miss a full request parsing
	*/
	s.CreateConfigMap(ns, "body-rules", `
SecRule ARGS|REQUEST_BODY "@contains DROP TABLE" "id:6001,phase:2,deny,status:403,msg:'SQL injection in body',log,auditlog"
SecRule ARGS|REQUEST_BODY "@contains <script>" "id:6002,phase:2,deny,status:403,msg:'XSS in body',log,auditlog"
SecRule ARGS|REQUEST_BODY "@contains malicious_payload" "id:6003,phase:2,deny,status:403,msg:'Malicious payload',log,auditlog"
`)
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "body-rules"})

	s.Step("create engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("deploy backend and route")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	s.Step("verify SQL injection in body is blocked")
	gw.ExpectPostBlocked("/api/data", "application/json", `{"query": "DROP TABLE users"}`)

	s.Step("verify XSS in body is blocked")
	gw.ExpectPostBlocked("/api/comment", "text/plain", `<script>alert('xss')</script>`)

	s.Step("verify malicious payload in form data is blocked")
	gw.ExpectPostBlocked("/api/submit", "application/x-www-form-urlencoded", "data=malicious_payload")

	s.Step("verify clean POST body is allowed")
	gw.ExpectPostAllowed("/api/data", "application/json", `{"name": "safe data"}`)
}

// TestRequestHeaderInspection verifies that malicious headers
// (User-Agent, Cookie, custom headers) get blocked by WAF rules.
func TestRequestHeaderInspection(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("header-inspection")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy rules for header inspection")
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On
SecDebugLogLevel 9
SecDebugLog /dev/stdout`)
	s.CreateConfigMap(ns, "header-rules", `
SecRule REQUEST_HEADERS:User-Agent "@contains sqlmap" "id:7001,phase:1,deny,status:403,msg:'SQLMap detected',log,auditlog"
SecRule REQUEST_HEADERS:User-Agent "@contains nikto" "id:7002,phase:1,deny,status:403,msg:'Nikto scanner detected',log,auditlog"
SecRule REQUEST_HEADERS:Cookie "@contains <script>" "id:7003,phase:1,deny,status:403,msg:'XSS in cookie',log,auditlog"
SecRule REQUEST_HEADERS:X-Custom-Header "@contains attack" "id:7004,phase:1,deny,status:403,msg:'Attack in custom header',log,auditlog"
SecRule REQUEST_HEADERS:Referer "@contains evil.com" "id:7005,phase:1,deny,status:403,msg:'Evil referer',log,auditlog"
`)
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "header-rules"})

	s.Step("create engine")
	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("deploy backend and route")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	s.Step("verify SQLMap user-agent is blocked")
	gw.ExpectHeaderBlocked("/", map[string]string{
		"User-Agent": "sqlmap/1.0",
	})

	s.Step("verify Nikto scanner is blocked")
	gw.ExpectHeaderBlocked("/", map[string]string{
		"User-Agent": "Mozilla/5.0 nikto",
	})

	s.Step("verify XSS in cookie is blocked")
	gw.ExpectHeaderBlocked("/", map[string]string{
		"Cookie": "session=<script>alert(1)</script>",
	})

	s.Step("verify malicious custom header is blocked")
	gw.ExpectHeaderBlocked("/", map[string]string{
		"X-Custom-Header": "this-is-an-attack",
	})

	s.Step("verify evil referer is blocked")
	gw.ExpectHeaderBlocked("/", map[string]string{
		"Referer": "https://evil.com/page",
	})

	s.Step("verify clean headers are allowed")
	gw.ExpectHeaderAllowed("/", map[string]string{
		"User-Agent":      "Mozilla/5.0 (compatible; safe browser)",
		"Cookie":          "session=abc123",
		"X-Custom-Header": "safe-value",
		"Referer":         "https://good.com/page",
	})
}
