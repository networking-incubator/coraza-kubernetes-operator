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

	"github.com/corazawaf/coraza/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

// useReleaseModule pins the release-build capability (no embedded CRS) for one
// test, so the rejection cases do not depend on the committed fixture.
func useReleaseModule(t *testing.T) {
	t.Helper()
	orig := BundledCRSVersion
	BundledCRSVersion = "none"
	t.Cleanup(func() { BundledCRSVersion = orig })
}

// useCRSModule simulates a module built with make wasm.crs for one test.
func useCRSModule(t *testing.T) {
	t.Helper()
	orig := BundledCRSVersion
	BundledCRSVersion = "v4.30.0"
	t.Cleanup(func() { BundledCRSVersion = orig })
}

// TestBundledCRSVersionDefaultsToManifest pins the default capability source:
// the committed fixture manifest, not a hard-coded value.
func TestBundledCRSVersionDefaultsToManifest(t *testing.T) {
	manifest, _, err := assets.ResolveDefault()
	require.NoError(t, err)
	assert.Equal(t, manifest.CRSVersion, BundledCRSVersion)
	assert.False(t, moduleEmbedsCRS(), "the committed fixture module embeds no CRS")
}

// TestModuleWithoutCRSRejectsCRSIncludes covers the release build: a crs/
// include resolves nothing in a module without embed-crs, so it must be
// rejected instead of compiling a zero-rule WAF while the operator reports
// Ready. Covers the RuleSource and RuleSet guard and the render entry point.
func TestModuleWithoutCRSRejectsCRSIncludes(t *testing.T) {
	useReleaseModule(t)

	for _, rules := range []string{
		"Include crs/crs-setup.conf",
		"Include crs/rules/*.conf",
		"Include crs/rules/REQUEST-901-INITIALIZATION.conf",
		"SecRuleEngine On\nInclude crs/crs-setup.conf\nInclude crs/rules/*.conf",
	} {
		t.Run(rules, func(t *testing.T) {
			require.ErrorIs(t, ValidateCorazaRSIncludes(rules, nil), errNoBundledCRSInclude)
			require.ErrorIs(t, ValidateIncludes(rules, nil), errNoBundledCRSInclude)

			err := ValidateRuleSourceRules(rules, "no-crs-rs", nil)
			require.Error(t, err, "the RuleSource entry point must reject it")
			assert.ErrorIs(t, err, errNoBundledCRSInclude)
			assert.Contains(t, err.Error(), "no-crs-rs")
		})
	}
}

// TestModuleWithoutCRSRejectsCRSData covers @pmFromFile references into the
// embedded CRS tree, in the rule text and in an included RuleData file.
func TestModuleWithoutCRSRejectsCRSData(t *testing.T) {
	useReleaseModule(t)

	err := ValidateIncludes(`SecRule ARGS "@pmFromFile crs/restricted-files.data" "id:1,phase:1,pass"`, nil)
	require.ErrorIs(t, err, errNoBundledCRSData)

	err = ValidateIncludes("Include tune.conf", map[string][]byte{
		"tune.conf": []byte(`SecRule ARGS "@pmf crs/scanners-user-agents.data" "id:1,phase:1,pass"` + "\n"),
	})
	require.ErrorIs(t, err, errNoBundledCRSData)
}

// TestModuleWithCRSAcceptsCRSReferences covers the CRS-enabled build: the
// same forms pass every guard.
func TestModuleWithCRSAcceptsCRSReferences(t *testing.T) {
	useCRSModule(t)

	for _, rules := range []string{
		"Include crs/crs-setup.conf",
		"Include crs/rules/*.conf",
		"Include crs/rules/REQUEST-901-INITIALIZATION.conf",
		"SecRuleEngine On\nInclude crs/crs-setup.conf\nInclude crs/rules/*.conf",
		`SecRule ARGS "@pmFromFile crs/restricted-files.data" "id:1,phase:1,pass"`,
	} {
		t.Run(rules, func(t *testing.T) {
			assert.NoError(t, ValidateIncludes(rules, nil))
			assert.NoError(t, ValidateRuleSourceRules(rules, "crs-rs", nil))
		})
	}
}

// TestCRSTuningByIDValidates covers aggregate validation of CRS tuning: the
// validation WAF compiles crs/ includes against empty stubs, so a directive
// tuning a CRS rule by ID needs a stand-in rule to compile at all.
func TestCRSTuningByIDValidates(t *testing.T) {
	useCRSModule(t)

	rules := "SecRuleEngine On\nInclude crs/crs-setup.conf\nInclude crs/rules/*.conf\n" +
		`SecRuleUpdateTargetById 942100 "ARGS:!password"` + "\n" +
		`SecRuleUpdateActionById 942100 "deny,status:403"` + "\n" +
		"SecRuleRemoveById 932110\n"

	require.NoError(t, ValidateIncludes(rules, nil))
	require.NoError(t, ValidateRuleSourceRules(rules, "crs-tune-rs", nil))

	waf, err := coraza.NewWAF(BuildWAFConfig(rules, nil))
	require.NoError(t, err, "a CRS rule tuned by ID must compile against a stand-in rule")
	CloseWAF(waf)
}

// TestCRSTuningOfLocallyDefinedRuleStillValidated covers an ID the tenant
// defines: no stand-in may be synthesized for it, or the compile fails with a
// duplicate rule id, and the update is checked against the real rule.
func TestCRSTuningOfLocallyDefinedRuleStillValidated(t *testing.T) {
	useCRSModule(t)

	rules := "Include crs/rules/*.conf\n" +
		`SecRule ARGS "@rx ." "id:100,phase:1,pass"` + "\n" +
		`SecRuleUpdateTargetById 100 "ARGS:foo"`
	require.NoError(t, ValidateRuleSourceRules(rules, "local-id-rs", nil))
}

// TestCRSTuningByIDInIncludedRuleData covers a tuning directive inside an
// included RuleData file: Coraza parses it as rule text, so the referenced ID
// and the crs/ include must be collected from it too.
func TestCRSTuningByIDInIncludedRuleData(t *testing.T) {
	useCRSModule(t)

	rules := "Include crs/rules/*.conf\nInclude tune.conf"
	dataFiles := map[string][]byte{
		"tune.conf": []byte(`SecRuleUpdateTargetById 942100 "ARGS:!password"` + "\n"),
	}
	require.NoError(t, ValidateIncludes(rules, dataFiles))
	require.NoError(t, ValidateRuleSourceRules(rules, "tune-rs", dataFiles))

	waf, err := coraza.NewWAF(BuildWAFConfig(rules, dataFiles))
	require.NoError(t, err)
	CloseWAF(waf)
}

// TestUnknownRuleIDStillFails covers the gate: without a crs/ include there is
// no CRS to tune, so an update-by-ID directive addressing an undefined rule
// keeps failing validation.
func TestUnknownRuleIDStillFails(t *testing.T) {
	useCRSModule(t)

	rules := `SecRule ARGS "@rx ." "id:1,phase:1,pass"` + "\n" +
		`SecRuleUpdateTargetById 999999 "ARGS:foo"`
	err := ValidateRuleSourceRules(rules, "unknown-id-rs", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "999999")
}
