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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCorazaRSIncludes(t *testing.T) {
	require.NoError(t, ValidateCorazaRSIncludes(`SecRuleEngine On
Include base.conf
Include "quoted.conf"`, map[string][]byte{
		"base.conf":   []byte("SecRuleEngine On"),
		"quoted.conf": []byte("SecRuleEngine On"),
	}))

	// The pinned CRS is embedded under crs/ by cargo's embed-crs feature.
	require.NoError(t, ValidateCorazaRSIncludes(`SecRuleEngine On
Include crs/crs-setup.conf
Include crs/rules/*.conf
Include crs/rules/REQUEST-901-INITIALIZATION.conf`, nil))

	for _, rules := range []string{
		"Include @owasp_crs/*.conf",
		"Include @recommended-conf",
		"Include @crs-setup-conf",
		"Include rules/*.conf",
		"Include crs/rules/*.data",
	} {
		assert.Error(t, ValidateCorazaRSIncludes(rules, nil), rules)
	}

	// A comment or a message string is not an Include directive.
	require.NoError(t, ValidateCorazaRSIncludes("# Include @owasp_crs/*.conf\nSecRule ARGS \"@contains Include @owasp_crs/*.conf\" \"id:1,phase:1,pass\"", nil))

	// An included RuleData file is parsed as rule text too.
	require.Error(t, ValidateCorazaRSIncludes("Include extra.conf", map[string][]byte{
		"extra.conf": []byte("Include @owasp_crs/*.conf"),
	}))
}

// The same crs/ forms must also pass the Go validation WAF, which stubs the
// embedded tree while it compiles the tenant rule text.
func TestCorazaRSCrsIncludesValidate(t *testing.T) {
	require.NoError(t, ValidateRuleSourceRules(`SecRuleEngine On
Include crs/crs-setup.conf
Include crs/rules/*.conf`, "crs", nil))
}
