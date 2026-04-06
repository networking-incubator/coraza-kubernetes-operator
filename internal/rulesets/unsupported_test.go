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

package rulesets

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryCompleteness(t *testing.T) {
	incompatible := IncompatibleRuleIDs()
	redundant := RedundantRuleIDs()
	total := len(incompatible) + len(redundant)

	assert.Equal(t, total, len(unsupportedRules), "total registry size should equal sum of tiers")
	assert.Len(t, incompatible, 46, "incompatible tier count")
	assert.Len(t, redundant, 21, "redundant tier count")
	assert.Equal(t, 67, total, "total unsupported rules")
}

func TestCheckUnsupportedRules_ResponseBodyInspection(t *testing.T) {
	responseBodyIDs := []int{
		950150,
		951110, 951120, 951130, 951140, 951150, 951160, 951170, 951180, 951190, 951200, 951210, 951220, 951230, 951240, 951250, 951260,
		952110,
		953101, 953120,
		954100, 954101, 954120,
		955100, 955110, 955120, 955260, 955400,
		956100, 956110,
	}

	for _, id := range responseBodyIDs {
		t.Run(fmt.Sprintf("rule_%d", id), func(t *testing.T) {
			found := CheckUnsupportedRules(secRuleWithID(id))
			require.Len(t, found, 1)
			assert.Equal(t, id, found[0].ID)
			assert.Equal(t, "response body inspection", found[0].Category)
			assert.Equal(t, TierIncompatible, found[0].Tier)
		})
	}
}

func TestCheckUnsupportedRules_MultipartCharsetDetection(t *testing.T) {
	found := CheckUnsupportedRules(secRuleWithID(922110))
	require.Len(t, found, 1)
	assert.Equal(t, 922110, found[0].ID)
	assert.Equal(t, "multipart charset detection", found[0].Category)
	assert.Equal(t, TierIncompatible, found[0].Tier)
}

func TestCheckUnsupportedRules_PL4FalsePositives(t *testing.T) {
	found := CheckUnsupportedRules(secRuleWithID(920274))
	require.Len(t, found, 1)
	assert.Equal(t, 920274, found[0].ID)
	assert.Equal(t, "PL4 false positives", found[0].Category)
	assert.Equal(t, TierIncompatible, found[0].Tier)
}

func TestCheckUnsupportedRules_CorazaWASMBugs(t *testing.T) {
	bugIDs := map[int]string{
		920171: "GET/HEAD body detection",
		920430: "protocol version detection",
		934120: "enclosed alphanumerics HTTP/2",
		932300: "multiphase evaluation",
		933120: "multiphase evaluation",
	}

	for id, expectedCategory := range bugIDs {
		t.Run(fmt.Sprintf("rule_%d", id), func(t *testing.T) {
			found := CheckUnsupportedRules(secRuleWithID(id))
			require.Len(t, found, 1)
			assert.Equal(t, id, found[0].ID)
			assert.Equal(t, expectedCategory, found[0].Category)
			assert.Equal(t, TierIncompatible, found[0].Tier)
		})
	}
}

func TestCheckUnsupportedRules_UnderInvestigation(t *testing.T) {
	underInvestigationIDs := []int{920270, 921140, 921250, 922130, 932200, 941210, 944200, 959100, 980170}

	for _, id := range underInvestigationIDs {
		t.Run(fmt.Sprintf("rule_%d", id), func(t *testing.T) {
			found := CheckUnsupportedRules(secRuleWithID(id))
			require.Len(t, found, 1)
			assert.Equal(t, id, found[0].ID)
			assert.Equal(t, "under investigation", found[0].Category)
			assert.Equal(t, TierIncompatible, found[0].Tier)
		})
	}
}

func TestCheckUnsupportedRules_HTTPMethodHandling(t *testing.T) {
	for _, id := range []int{911100, 920100, 949110} {
		t.Run(fmt.Sprintf("rule_%d", id), func(t *testing.T) {
			found := CheckUnsupportedRules(secRuleWithID(id))
			require.Len(t, found, 1)
			assert.Equal(t, id, found[0].ID)
			assert.Equal(t, "HTTP method handling", found[0].Category)
			assert.Equal(t, TierRedundant, found[0].Tier)
		})
	}
}

func TestCheckUnsupportedRules_ProtocolEnforcement(t *testing.T) {
	for _, id := range []int{920181, 920210} {
		t.Run(fmt.Sprintf("rule_%d", id), func(t *testing.T) {
			found := CheckUnsupportedRules(secRuleWithID(id))
			require.Len(t, found, 1)
			assert.Equal(t, id, found[0].ID)
			assert.Equal(t, "protocol enforcement", found[0].Category)
			assert.Equal(t, TierRedundant, found[0].Tier)
		})
	}
}

func TestCheckUnsupportedRules_HTTP2Normalization(t *testing.T) {
	for _, id := range []int{920280, 920290, 920300, 920310, 920311, 920320, 920330, 920610} {
		t.Run(fmt.Sprintf("rule_%d", id), func(t *testing.T) {
			found := CheckUnsupportedRules(secRuleWithID(id))
			require.Len(t, found, 1)
			assert.Equal(t, id, found[0].ID)
			assert.Equal(t, "HTTP/2 normalization", found[0].Category)
			assert.Equal(t, TierRedundant, found[0].Tier)
		})
	}
}

func TestCheckUnsupportedRules_HeaderSanitization(t *testing.T) {
	for _, id := range []int{932161, 932207, 932237, 932239, 941101, 941110, 941120, 942280} {
		t.Run(fmt.Sprintf("rule_%d", id), func(t *testing.T) {
			found := CheckUnsupportedRules(secRuleWithID(id))
			require.Len(t, found, 1)
			assert.Equal(t, id, found[0].ID)
			assert.Equal(t, "header sanitization", found[0].Category)
			assert.Equal(t, TierRedundant, found[0].Tier)
		})
	}
}

func TestCheckUnsupportedRules_AllIncompatibleIDs(t *testing.T) {
	for _, id := range IncompatibleRuleIDs() {
		t.Run(fmt.Sprintf("rule_%d", id), func(t *testing.T) {
			found := CheckUnsupportedRules(secRuleWithID(id))
			require.Len(t, found, 1, "incompatible rule %d should be detected", id)
			assert.Equal(t, id, found[0].ID)
			assert.Equal(t, TierIncompatible, found[0].Tier)
		})
	}
}

func TestCheckUnsupportedRules_AllRedundantIDs(t *testing.T) {
	for _, id := range RedundantRuleIDs() {
		t.Run(fmt.Sprintf("rule_%d", id), func(t *testing.T) {
			found := CheckUnsupportedRules(secRuleWithID(id))
			require.Len(t, found, 1, "redundant rule %d should be detected", id)
			assert.Equal(t, id, found[0].ID)
			assert.Equal(t, TierRedundant, found[0].Tier)
		})
	}
}

func TestCheckUnsupportedRules_Clean(t *testing.T) {
	tests := []struct {
		name  string
		rules string
	}{
		{
			name:  "supported rule",
			rules: `SecRule REQUEST_URI "@contains /admin" "id:1,phase:1,deny"`,
		},
		{
			name:  "multiple supported rules",
			rules: secRulesWithIDs(1, 2, 3, 100, 200),
		},
		{
			name:  "directive without id",
			rules: `SecDefaultAction "phase:1,log,auditlog,pass"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found := CheckUnsupportedRules(tt.rules)
			assert.Nil(t, found)
		})
	}
}

func TestCheckUnsupportedRules_EmptyInput(t *testing.T) {
	assert.Nil(t, CheckUnsupportedRules(""))
}

func TestCheckUnsupportedRules_NoIDs(t *testing.T) {
	rules := "SecRuleEngine On\nSecRequestBodyAccess On\nSecResponseBodyAccess On"
	assert.Nil(t, CheckUnsupportedRules(rules))
}

func TestCheckUnsupportedRules_MixedSupportedAndUnsupported(t *testing.T) {
	rules := secRulesWithIDs(1, 950150, 2, 922110, 3)
	found := CheckUnsupportedRules(rules)
	require.Len(t, found, 2)
	assert.Equal(t, 922110, found[0].ID)
	assert.Equal(t, 950150, found[1].ID)
}

func TestCheckUnsupportedRules_DuplicateIDsDeduped(t *testing.T) {
	rules := secRuleWithID(950150) + "\n" + secRuleWithID(950150)
	found := CheckUnsupportedRules(rules)
	require.Len(t, found, 1)
	assert.Equal(t, 950150, found[0].ID)
}

func TestCheckUnsupportedRules_CommentedOutRulesIgnored(t *testing.T) {
	rules := `# SecRule RESPONSE_BODY "@rx error" "id:950150,phase:4,deny"
SecRule REQUEST_URI "@contains /admin" "id:1,phase:1,deny"`
	found := CheckUnsupportedRules(rules)
	assert.Nil(t, found, "commented-out unsupported rule should be ignored")
}

func TestCheckUnsupportedRules_MixedCommentAndActive(t *testing.T) {
	rules := `# This rule is disabled:
# SecRule RESPONSE_BODY "@rx error" "id:950150,phase:4,deny"
SecRule REQUEST_URI "@rx test" "id:911100,phase:1,pass"`
	found := CheckUnsupportedRules(rules)
	require.Len(t, found, 1)
	assert.Equal(t, 911100, found[0].ID, "only the active unsupported rule should be detected")
}

func TestCheckUnsupportedRules_QuotedIDs(t *testing.T) {
	rules := `SecRule REQUEST_URI "@rx test" "id:'950150',phase:1,pass"`
	found := CheckUnsupportedRules(rules)
	require.Len(t, found, 1)
	assert.Equal(t, 950150, found[0].ID)
}

func TestCheckUnsupportedRules_MixedTiers(t *testing.T) {
	rules := secRulesWithIDs(950150, 911100)
	found := CheckUnsupportedRules(rules)
	require.Len(t, found, 2)

	assert.Equal(t, TierRedundant, found[0].Tier)
	assert.Equal(t, TierIncompatible, found[1].Tier)
}

func TestFormatUnsupportedMessage_Empty(t *testing.T) {
	assert.Equal(t, "", FormatUnsupportedMessage(nil))
	assert.Equal(t, "", FormatUnsupportedMessage([]UnsupportedRule{}))
}

func TestFormatUnsupportedMessage_SingleRule(t *testing.T) {
	found := []UnsupportedRule{
		{ID: 950150, Category: "response body inspection", Tier: TierIncompatible, Description: "response body inspection is not supported in Envoy WASM"},
	}
	msg := FormatUnsupportedMessage(found)
	assert.Contains(t, msg, "1 unsupported rule(s)")
	assert.Contains(t, msg, "rule 950150")
	assert.Contains(t, msg, "response body inspection")
	assert.Contains(t, msg, "LIMITATIONS.md")
}

func TestFormatUnsupportedMessage_MultipleRules(t *testing.T) {
	found := []UnsupportedRule{
		{ID: 922110, Category: "multipart charset detection", Tier: TierIncompatible, Description: "multipart charset detection does not function"},
		{ID: 950150, Category: "response body inspection", Tier: TierIncompatible, Description: "response body inspection not supported"},
	}
	msg := FormatUnsupportedMessage(found)
	assert.Contains(t, msg, "2 unsupported rule(s)")
	assert.Contains(t, msg, "rule 922110")
	assert.Contains(t, msg, "rule 950150")
}

// -----------------------------------------------------------------------------
// Test Helpers
// -----------------------------------------------------------------------------

// secRuleWithID returns a minimal SecLang rule with the given ID.
func secRuleWithID(id int) string {
	return fmt.Sprintf(`SecRule REQUEST_URI "@rx test" "id:%d,phase:1,pass,nolog"`, id)
}

// secRulesWithIDs returns one rule per ID, joined by newlines.
func secRulesWithIDs(ids ...int) string {
	var rules strings.Builder
	for i, id := range ids {
		if i > 0 {
			rules.WriteString("\n")
		}
		rules.WriteString(secRuleWithID(id))
	}
	return rules.String()
}
