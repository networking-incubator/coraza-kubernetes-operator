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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRulePolicy(t *testing.T) {
	tests := []struct {
		name               string
		operators          string
		directives         string
		maxRequestBody     int64
		responseBodyAccess string
		want               RulePolicy
		wantErr            string
	}{
		{
			name:           "flag defaults deny nothing",
			maxRequestBody: MaxRequestBodyLimit,
			want:           RulePolicy{},
		},
		{
			name:               "names are normalised to the registered spelling",
			operators:          "PMFROMFILE, pmf ,rx,ipMatchF,geoLookup",
			directives:         "secruleengine,include",
			maxRequestBody:     1024,
			responseBodyAccess: ResponseBodyAccessDenied,
			want: RulePolicy{
				DeniedOperators:        []string{"pmFromFile", "pmFromFile", "rx", "ipMatchFromFile", "unconditionalMatch"},
				DeniedDirectives:       []string{"SecRuleEngine", "Include"},
				MaxRequestBodyLimit:    1024,
				DenyResponseBodyAccess: true,
			},
		},
		{
			name:           "unknown operator",
			operators:      "nope",
			maxRequestBody: MaxRequestBodyLimit,
			wantErr:        `rule-denied-operators: "nope" is not an operator envoy-coraza supports`,
		},
		{
			name:           "operator the gateway module does not support",
			operators:      "rbl",
			maxRequestBody: MaxRequestBodyLimit,
			wantErr:        `rule-denied-operators: "rbl" is not an operator envoy-coraza supports`,
		},
		{
			name:           "empty operator entry",
			operators:      "rx,,pm",
			maxRequestBody: MaxRequestBodyLimit,
			wantErr:        `rule-denied-operators: empty entry in "rx,,pm"`,
		},
		{
			name:           "empty directive entry",
			directives:     " , ",
			maxRequestBody: MaxRequestBodyLimit,
			wantErr:        `rule-denied-directives: empty entry in " , "`,
		},
		{
			name:           "not a directive name",
			directives:     "SecRule Engine",
			maxRequestBody: MaxRequestBodyLimit,
			wantErr:        `rule-denied-directives: "SecRule Engine" is not a directive envoy-coraza supports`,
		},
		{
			name:           "directive envoy-coraza ignores",
			directives:     "SecRuleScript",
			maxRequestBody: MaxRequestBodyLimit,
			wantErr:        `rule-denied-directives: "SecRuleScript" is not a directive envoy-coraza supports`,
		},
		{
			name:    "zero body limit",
			wantErr: "rule-max-request-body-limit 0 must be within (0, 16777216] bytes",
		},
		{
			name:           "body limit above the envoy-coraza ceiling",
			maxRequestBody: MaxRequestBodyLimit + 1,
			wantErr:        "rule-max-request-body-limit 16777217 must be within (0, 16777216] bytes",
		},
		{
			name:               "unknown response body access",
			maxRequestBody:     MaxRequestBodyLimit,
			responseBodyAccess: "denied",
			wantErr:            `rule-response-body-access "denied" must be Allowed or Denied`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responseBodyAccess := tt.responseBodyAccess
			if responseBodyAccess == "" {
				responseBodyAccess = ResponseBodyAccessAllowed
			}
			got, err := ParseRulePolicy(tt.operators, tt.directives, tt.maxRequestBody, responseBodyAccess)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCheckRulePolicy(t *testing.T) {
	const (
		pmDenied       = "@pm is not allowed by the cluster rule policy"
		directiveError = "SecRuleEngine is not allowed by the cluster rule policy"
		bodyLimitError = "SecRequestBodyLimit 2048 exceeds the cluster rule policy maximum of 1024 bytes"
		responseError  = "SecResponseBodyAccess On is not allowed by the cluster rule policy"
	)
	policy := RulePolicy{
		DeniedOperators:        []string{"pm", "pmFromFile", "rx", "unconditionalMatch"},
		DeniedDirectives:       []string{"SecRuleEngine", "SecRuleRemoveById", "SecRuleRemoveByTag", "SecRuleUpdateTargetById"},
		MaxRequestBodyLimit:    1024,
		DenyResponseBodyAccess: true,
	}
	included := func(text string) map[string][]byte {
		return map[string][]byte{"extra.conf": []byte(text)}
	}

	tests := []struct {
		name      string
		rules     string
		dataFiles map[string][]byte
		wantErr   string
	}{
		{name: "denied operator", rules: `SecRule ARGS "@pm a b" "id:1,phase:1,deny"`, wantErr: pmDenied},
		{name: "negated denied operator", rules: `SecRule ARGS "!@pm a" "id:1,phase:1,deny"`, wantErr: pmDenied},
		{name: "denied operator in another casing", rules: `SecRule ARGS "@PM a" "id:1,phase:1,deny"`, wantErr: pmDenied},
		{name: "denied operator alias", rules: `SecRule ARGS "@pmf a.data" "id:1,phase:1,deny"`, wantErr: "@pmFromFile is not allowed by the cluster rule policy"},
		{name: "geoLookup is unconditionalMatch", rules: `SecRule REMOTE_ADDR "@geoLookup" "id:1,phase:1,deny"`, wantErr: "@unconditionalMatch is not allowed by the cluster rule policy"},
		{name: "default operator", rules: `SecRule ARGS "attack" "id:1,phase:1,deny"`, wantErr: "@rx is not allowed by the cluster rule policy"},
		{name: "denied operator in a chained rule", rules: "SecRule ARGS \"@contains a\" \"id:1,phase:1,deny,chain\"\n  SecRule ARGS \"@pm b\"", wantErr: pmDenied},
		{name: "allowed operator", rules: `SecRule ARGS "@contains a" "id:1,phase:1,deny,msg:'@pm in a message'"`},
		{name: "denied directive", rules: "secruleengine On", wantErr: directiveError},
		{name: "directive name in an action", rules: `SecAction "id:1,phase:1,pass,msg:'SecRuleEngine'"`},
		{name: "request body limit above the policy", rules: "SecRequestBodyLimit 2048", wantErr: bodyLimitError},
		{name: "request body limit at the policy", rules: "SecRequestBodyLimit 1024"},
		{name: "response body access on", rules: `SecResponseBodyAccess "On"`, wantErr: responseError},
		{name: "response body access off", rules: "SecResponseBodyAccess Off"},
		{name: "operator in an included RuleData file", rules: "Include extra.conf", dataFiles: included(`SecRule ARGS "@pm a" "id:1,phase:1,deny"`), wantErr: "included RuleData file extra.conf: " + pmDenied},
		{name: "directive in an included RuleData file", rules: "Include extra.conf", dataFiles: included("SecRuleEngine On"), wantErr: "included RuleData file extra.conf: " + directiveError},
		{name: "body limit in an included RuleData file", rules: "Include extra.conf", dataFiles: included("SecRequestBodyLimit 2048"), wantErr: "included RuleData file extra.conf: " + bodyLimitError},
		{name: "response body access in an included RuleData file", rules: "Include extra.conf", dataFiles: included("SecResponseBodyAccess On"), wantErr: "included RuleData file extra.conf: " + responseError},

		// A ctl action is judged like the directive with the same effect.
		{name: "ctl ruleEngine", rules: `SecAction "id:1,phase:1,pass,nolog,ctl:ruleEngine=Off"`, wantErr: "ctl:ruleEngine is not allowed by the cluster rule policy: it acts like SecRuleEngine"},
		{name: "ctl in another casing", rules: `SecAction "id:1,phase:1,pass,CTL:RuleEngine=Off"`, wantErr: "ctl:ruleEngine is not allowed by the cluster rule policy: it acts like SecRuleEngine"},
		{name: "ctl ruleRemoveById in a rule", rules: `SecRule ARGS "@contains a" "id:1,phase:1,pass,ctl:ruleRemoveById=1-999999"`, wantErr: "ctl:ruleRemoveById is not allowed by the cluster rule policy: it acts like SecRuleRemoveById"},
		{name: "ctl ruleRemoveByTag in default actions", rules: `SecDefaultAction "phase:1,log,pass,ctl:ruleRemoveByTag=attack"`, wantErr: "ctl:ruleRemoveByTag is not allowed by the cluster rule policy: it acts like SecRuleRemoveByTag"},
		{name: "ctl ruleRemoveTargetById in an action update", rules: `SecRuleUpdateActionById 1 "ctl:ruleRemoveTargetById=2;ARGS:q"`, wantErr: "ctl:ruleRemoveTargetById is not allowed by the cluster rule policy: it acts like SecRuleUpdateTargetById"},
		{name: "ctl responseBodyAccess on", rules: `SecAction "id:1,phase:1,pass,ctl:'responseBodyAccess=On'"`, wantErr: "ctl:responseBodyAccess=On is not allowed by the cluster rule policy"},
		{name: "ctl requestBodyLimit above the policy", rules: `SecAction "id:1,phase:1,pass,ctl:requestBodyLimit=16777216"`, wantErr: "ctl:requestBodyLimit=16777216 exceeds the cluster rule policy maximum of 1024 bytes"},
		{name: "ctl requestBodyLimit at the policy", rules: `SecAction "id:1,phase:1,pass,ctl:requestBodyLimit=1024"`},
		{name: "ctl requestBodyLimit above the envoy-coraza ceiling", rules: `SecAction "id:1,phase:1,pass,ctl:requestBodyLimit=20000000"`, wantErr: "ctl:requestBodyLimit=20000000 exceeds the cluster rule policy maximum of 1024 bytes"},
		{name: "ctl requestBodyAccess without a limit", rules: `SecAction "id:1,phase:1,pass,ctl:requestBodyAccess=On"`, wantErr: "ctl:requestBodyAccess=On needs a SecRequestBodyLimit at or below the cluster rule policy maximum of 1024 bytes"},
		{name: "ctl requestBodyAccess with a limit", rules: "SecRequestBodyLimit 1024\n" + `SecAction "id:1,phase:1,pass,ctl:requestBodyAccess=On"`},
		{name: "ctl in an action update for several rules", rules: `SecRuleUpdateActionById 1 2 "ctl:ruleEngine=Off"`, wantErr: "ctl:ruleEngine is not allowed by the cluster rule policy: it acts like SecRuleEngine"},
		{name: "ctl in an action update after a tab", rules: "SecRuleUpdateActionById 1\t\"ctl:ruleEngine=Off\"", wantErr: "ctl:ruleEngine is not allowed by the cluster rule policy: it acts like SecRuleEngine"},
		{name: "ctl in the quoted actions envoy-coraza reads", rules: `SecRuleUpdateActionById 1 "msg:'a b',ctl:ruleEngine=Off"`, wantErr: "ctl:ruleEngine is not allowed by the cluster rule policy: it acts like SecRuleEngine"},
		{name: "ctl ruleRemoveTargetByMsg", rules: `SecAction "id:1,phase:1,pass,ctl:ruleRemoveTargetByMsg=m;ARGS:q"`, wantErr: "ctl:ruleRemoveTargetByMsg is not allowed by the cluster rule policy: it acts like SecRuleUpdateTargetById"},
		{name: "ctl text in a message", rules: `SecAction "id:1,phase:1,pass,msg:'ctl:ruleEngine=Off'"`},
		{name: "ctl in an included RuleData file", rules: "Include extra.conf", dataFiles: included(`SecAction "id:1,phase:1,pass,ctl:ruleEngine=Off"`), wantErr: "included RuleData file extra.conf: ctl:ruleEngine is not allowed by the cluster rule policy: it acts like SecRuleEngine"},
		{name: "ctl requestBodyAccess with a limit in an included RuleData file", rules: "Include extra.conf\n" + `SecAction "id:1,phase:1,pass,ctl:requestBodyAccess=On"`, dataFiles: included("SecRequestBodyLimit 1024")},

		// SecDataset items are data, not directives.
		{name: "dataset item named like a directive", rules: "SecDataset php `\ninclude\nSecRuleEngine Off\n`\n" + `SecRule ARGS "@contains a" "id:1,phase:2,deny"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckRulePolicy(tt.rules, tt.dataFiles, policy)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
			assert.NoError(t, CheckRulePolicy(tt.rules, tt.dataFiles, RulePolicy{}), "an empty policy denies nothing")
		})
	}
}

// TestCtlRuleRemoveTargetByMsg covers the target update envoy-coraza only has
// as a ctl action: denying either target update directive denies it.
func TestCtlRuleRemoveTargetByMsg(t *testing.T) {
	rules := `SecAction "id:1,phase:1,pass,ctl:ruleRemoveTargetByMsg=m;ARGS:q"`
	for _, directive := range []string{"SecRuleUpdateTargetById", "SecRuleUpdateTargetByTag"} {
		require.ErrorContains(t, CheckRulePolicy(rules, nil, RulePolicy{DeniedDirectives: []string{directive}}), "it acts like "+directive)
	}
	require.NoError(t, CheckRulePolicy(rules, nil, RulePolicy{DeniedDirectives: []string{"SecRuleEngine"}}))
}

// TestCtlInActionUpdateWithUnbalancedQuotes covers SecRuleUpdateActionById
// arguments whose double quotes do not pair up: both engines strip them and
// apply the ctl action, so the policy reads the action the same way.
func TestCtlInActionUpdateWithUnbalancedQuotes(t *testing.T) {
	policy := RulePolicy{
		DeniedDirectives:       []string{"SecRuleEngine", "SecRuleRemoveById"},
		MaxRequestBodyLimit:    1024,
		DenyResponseBodyAccess: true,
	}
	ctls := map[string]string{
		"ctl:ruleEngine=Off":            "ctl:ruleEngine is not allowed by the cluster rule policy",
		"ctl:ruleRemoveById=1-99999":    "ctl:ruleRemoveById is not allowed by the cluster rule policy",
		"ctl:responseBodyAccess=On":     "ctl:responseBodyAccess=On is not allowed by the cluster rule policy",
		"ctl:requestBodyLimit=16000000": "ctl:requestBodyLimit=16000000 exceeds the cluster rule policy maximum of 1024 bytes",
		"ctl:requestBodyAccess=On":      "ctl:requestBodyAccess=On needs a SecRequestBodyLimit",
	}
	for _, form := range []string{`1 "%s`, `1 2 "%s`, `"1 "%s""`} {
		for ctl, want := range ctls {
			rules := "SecRuleUpdateActionById " + fmt.Sprintf(form, ctl)
			require.ErrorContains(t, CheckRulePolicy(rules, nil, policy), want, rules)
		}
	}
}

// TestOperatorListsDoNotOverlap keeps an operator the gateway module lacks out
// of the names the cluster rule policy accepts.
func TestOperatorListsDoNotOverlap(t *testing.T) {
	for _, name := range unsupportedOperators {
		assert.NotContains(t, operatorNames, strings.ToLower(name))
	}
}
