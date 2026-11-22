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

package validation

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGatewayListsMatchGatewayModulePin fails when the gateway module pin
// moves, so the lists of what envoy-coraza supports are re-checked against it.
func TestGatewayListsMatchGatewayModulePin(t *testing.T) {
	raw, err := os.ReadFile("../../../Makefile")
	require.NoError(t, err)
	match := regexp.MustCompile(`(?m)^CORAZA_RS_VERSION \?= (\S+)$`).FindSubmatch(raw)
	require.NotNil(t, match, "the Makefile must pin CORAZA_RS_VERSION")
	assert.Equal(t, gatewayModuleCheckedAt, string(match[1]),
		"the gateway module changed: re-check the lists in this package against it, then update gatewayModuleCheckedAt")
}

// TestUnsupportedOperatorsAreRejected covers the validation WAF, which the
// RuleSource and the aggregate RuleSet compilation build the same way.
func TestUnsupportedOperatorsAreRejected(t *testing.T) {
	const rbl = "@rbl is not supported by the envoy-coraza plugin"
	tests := []struct {
		name      string
		rules     string
		dataFiles map[string][]byte
		wantErr   string
	}{
		{
			name:    "rbl",
			rules:   `SecRule REMOTE_ADDR "@rbl sbl.spamhaus.org" "id:1,phase:1,deny"`,
			wantErr: rbl,
		},
		{
			name:    "rbl without an argument",
			rules:   `SecRule REMOTE_ADDR "@rbl" "id:1,phase:1,deny"`,
			wantErr: rbl,
		},
		{
			name:    "negated rbl",
			rules:   `SecRule REMOTE_ADDR "!@rbl sbl.spamhaus.org" "id:1,phase:1,deny"`,
			wantErr: rbl,
		},
		{
			name:    "inspectFile",
			rules:   `SecRule FILES_TMPNAMES "@inspectFile /bin/true" "id:1,phase:2,deny"`,
			wantErr: "@inspectFile is not supported by the envoy-coraza plugin",
		},
		{
			name:    "validateSchema",
			rules:   `SecRule REQUEST_BODY "@validateSchema schema.json" "id:1,phase:2,deny"`,
			wantErr: "@validateSchema is not supported by the envoy-coraza plugin",
		},
		{
			// Coraza requires the operator to be quoted.
			name:    "unquoted rbl",
			rules:   `SecRule REMOTE_ADDR @rbl "id:1,phase:1,deny"`,
			wantErr: "invalid operator for rule with operator",
		},
		{
			name:    "rbl in a chained rule",
			rules:   "SecRule REQUEST_URI \"@beginsWith /\" \"id:1,phase:1,deny,chain\"\n  SecRule REMOTE_ADDR \"@rbl sbl.spamhaus.org\"",
			wantErr: rbl,
		},
		{
			name:    "rbl across a line continuation",
			rules:   "SecRule REMOTE_ADDR \\\n  \"@rbl sbl.spamhaus.org\" \"id:1,phase:1,deny\"",
			wantErr: rbl,
		},
		{
			name:  "rbl in an included RuleData file",
			rules: "Include extra.conf",
			dataFiles: map[string][]byte{
				"extra.conf": []byte(`SecRule REMOTE_ADDR "@rbl sbl.spamhaus.org" "id:1,phase:1,deny"`),
			},
			wantErr: rbl,
		},
		{
			// Coraza looks operators up by exact name.
			name:    "rbl in another casing",
			rules:   `SecRule REMOTE_ADDR "@RBL sbl.spamhaus.org" "id:1,phase:1,deny"`,
			wantErr: "operator RBL not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRuleSourceRules(tt.rules, "operator-rs", tt.dataFiles)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "RuleSource operator-rs doesn't contain valid rules")
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestSupportedOperatorsCompile compiles a CRS-shaped rule set that uses every
// operator envoy-coraza supports, so the unsupported list removes nothing else
// and every name the cluster rule policy accepts is one Coraza registers.
func TestSupportedOperatorsCompile(t *testing.T) {
	// SecDataset blocks are backtick-quoted, so they cannot sit in the raw string.
	rules := "SecDataset words `\nattack\n`\nSecDataset nets `\n10.0.0.0/8\n`\n" + `
SecRuleEngine On
SecRequestBodyAccess On
SecRequestBodyLimit 13107200
SecDefaultAction "phase:1,log,auditlog,pass"
SecDefaultAction "phase:2,log,auditlog,pass"
SecAction "id:900000,phase:1,pass,nolog,setvar:tx.blocking_paranoia_level=1"
SecRule TX:BLOCKING_PARANOIA_LEVEL "@lt 1" "id:911011,phase:1,pass,nolog,skipAfter:END-911"
SecRule REQUEST_METHOD "!@within GET HEAD POST" "id:911100,phase:1,block,t:none"
SecMarker "END-911"
SecRule ARGS "@detectSQLi" "id:942100,phase:2,block,t:none,t:urlDecodeUni"
SecRule ARGS "@detectXSS" "id:941100,phase:2,block,t:none,t:htmlEntityDecode"
SecRule ARGS "@rx (?i)union\s+select" "id:942101,phase:2,block,chain"
  SecRule REQUEST_HEADERS:User-Agent "@pm curl wget" "t:lowercase"
SecRule REQUEST_URI "@restpath /users/{id}" "id:2,phase:1,pass"
SecRule REQUEST_URI "@pmFromFile words.data" "id:3,phase:1,pass"
SecRule REQUEST_URI "@pmf words.data" "id:4,phase:1,pass"
SecRule REQUEST_URI "@pmFromDataset words" "id:5,phase:1,pass"
SecRule REQUEST_URI "@strmatch attack" "id:6,phase:1,pass"
SecRule REQUEST_URI "@eq 1" "id:7,phase:1,pass"
SecRule REQUEST_URI "@le 1" "id:8,phase:1,pass"
SecRule REQUEST_URI "@gt 1" "id:9,phase:1,pass"
SecRule REQUEST_URI "@ge 1" "id:10,phase:1,pass"
SecRule REQUEST_URI "@streq /" "id:11,phase:1,pass"
SecRule REQUEST_URI "@contains admin" "id:12,phase:1,pass"
SecRule REQUEST_URI "@beginsWith /api" "id:13,phase:1,pass"
SecRule REQUEST_URI "@endsWith .php" "id:14,phase:1,pass"
SecRule REMOTE_ADDR "@ipMatch 127.0.0.1" "id:15,phase:1,pass"
SecRule REMOTE_ADDR "@ipMatchFromFile nets.data" "id:16,phase:1,pass"
SecRule REMOTE_ADDR "@ipMatchF nets.data" "id:17,phase:1,pass"
SecRule REMOTE_ADDR "@ipMatchFromDataset nets" "id:18,phase:1,pass"
SecRule ARGS "@validateByteRange 32-126" "id:19,phase:2,pass"
SecRule ARGS "@validateUrlEncoding" "id:20,phase:2,pass"
SecRule ARGS "@validateUtf8Encoding" "id:21,phase:2,pass"
SecRule ARGS "@validateNid us [0-9]{9}" "id:22,phase:2,pass"
SecRule REMOTE_ADDR "@unconditionalMatch" "id:23,phase:1,pass"
SecRule REMOTE_ADDR "@geoLookup" "id:24,phase:1,pass"
SecRule REMOTE_ADDR "@noMatch" "id:25,phase:1,pass"
SecRule REQUEST_URI "/admin" "id:26,phase:1,pass"
`
	dataFiles := map[string][]byte{
		"words.data": []byte("attack\n"),
		"nets.data":  []byte("10.0.0.0/8\n"),
	}

	require.NoError(t, ValidateRuleSourceRules(rules, "crs-like", dataFiles))
	// The cluster rule policy accepts exactly these names.
	for _, name := range supportedOperators {
		assert.Regexp(t, `"!?@`+name+`[ "]`, rules, "the rule set must use @%s", name)
	}
}

// TestUnsupportedOperatorBehindMissingFile covers a RuleSource whose per-source
// check stops at a RuleData reference it cannot resolve: the aggregate RuleSet
// validation, which has the RuleData files, still rejects the operator.
func TestUnsupportedOperatorBehindMissingFile(t *testing.T) {
	rules := `SecRule ARGS "@pmFromFile words.data" "id:1,phase:1,deny"` + "\n" +
		`SecRule REMOTE_ADDR "@rbl sbl.spamhaus.org" "id:2,phase:1,deny"`

	require.NoError(t, ValidateRuleSourceRules(rules, "masked-rs", nil), "the per-source check defers the missing file")
	require.ErrorContains(t, ValidateRuleSourceRules(rules, "masked-rs", map[string][]byte{"words.data": []byte("attack\n")}),
		"@rbl is not supported by the envoy-coraza plugin")
}
