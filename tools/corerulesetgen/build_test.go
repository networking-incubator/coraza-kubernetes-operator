package corerulesetgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuild_minimal_statsAndConfResults(t *testing.T) {
	dir := filepath.Join("testdata", "minimal", "rules")
	ver := mustParseCRSVersion(t, "4.24.1")
	scan, err := Scan(dir)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:       dir,
		Version:        "4.24.1",
		RuleSetName:    "default-ruleset",
		DataSourceName: "coreruleset-data",
	}, scan, ver)
	require.NoError(t, err)

	require.Equal(t, 1, bundle.Stats.Processed)
	require.Equal(t, 1, bundle.Stats.Skipped)
	require.Len(t, bundle.ExtraRuleSources, 1)
	require.Equal(t, "simple", bundle.ExtraRuleSources[0].Name)
	require.Empty(t, bundle.DataRuleDataDoc)

	require.Len(t, bundle.ConfFileResults, 2)
	require.Equal(t, "empty.conf", bundle.ConfFileResults[0].BaseName)
	require.Empty(t, bundle.ConfFileResults[0].YAML)
	require.NotEmpty(t, bundle.ConfFileResults[0].SkipReason)
	require.Equal(t, "simple.conf", bundle.ConfFileResults[1].BaseName)
	require.NotEmpty(t, bundle.ConfFileResults[1].YAML)
	require.Equal(t, "simple", bundle.ConfFileResults[1].SourceName)

	require.Contains(t, bundle.BaseRuleSourceYAML, "ver:'OWASP_CRS/4.24.1'")
	require.Contains(t, bundle.RuleSetDoc, "name: default-ruleset")
	require.Contains(t, bundle.RuleSetDoc, "- name: base-rules")
	require.Contains(t, bundle.RuleSetDoc, "- name: simple")
}

func TestBuild_emptyRulesDirectory(t *testing.T) {
	tmp := t.TempDir()
	ver := mustParseCRSVersion(t, "4.0.0")
	scan, err := Scan(tmp)
	require.NoError(t, err)
	require.Empty(t, scan.ConfPaths)
	require.Empty(t, scan.DataPaths)

	bundle, err := Build(Options{
		RulesDir:       tmp,
		Version:        "4.0.0",
		RuleSetName:    "only-base",
		DataSourceName: "coreruleset-data",
	}, scan, ver)
	require.NoError(t, err)

	require.Empty(t, bundle.ExtraRuleSources)
	require.Empty(t, bundle.ConfFileResults)
	require.Equal(t, 0, bundle.Stats.Processed)
	require.Equal(t, 0, bundle.Stats.Skipped)
	require.Empty(t, bundle.DataRuleDataDoc)
	require.Contains(t, bundle.RuleSetDoc, "name: only-base")
	require.Contains(t, bundle.RuleSetDoc, "- name: base-rules")
}

func TestBuild_namespaceInjectedInBaseRuleSource(t *testing.T) {
	tmp := t.TempDir()
	ver := mustParseCRSVersion(t, "4.1.0")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:       tmp,
		Version:        "4.1.0",
		Namespace:      "waf-system",
		RuleSetName:    "rs",
		DataSourceName: "data",
	}, scan, ver)
	require.NoError(t, err)

	require.Contains(t, bundle.BaseRuleSourceYAML, "namespace: waf-system")
	require.Contains(t, bundle.RuleSetDoc, "namespace: waf-system")
}

func TestBuild_includeTestRuleAddsTestBlock(t *testing.T) {
	tmp := t.TempDir()
	ver := mustParseCRSVersion(t, "4.0.0")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:        tmp,
		Version:         "4.0.0",
		IncludeTestRule: true,
		RuleSetName:     "rs",
		DataSourceName:  "coreruleset-data",
	}, scan, ver)
	require.NoError(t, err)

	require.Contains(t, bundle.BaseRuleSourceYAML, "id:999999")
	require.Contains(t, bundle.BaseRuleSourceYAML, "X-CRS-Test")
}

func TestBuild_withDataFiles_emitsDataRuleSource(t *testing.T) {
	rulesPath := filepath.Join("testdata", "withdata", "rules")
	ver := mustParseCRSVersion(t, "4.0.0")
	scan, err := Scan(rulesPath)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:       rulesPath,
		Version:        "4.0.0",
		RuleSetName:    "default-ruleset",
		DataSourceName: "coreruleset-data",
	}, scan, ver)
	require.NoError(t, err)

	require.NotEmpty(t, bundle.DataRuleDataDoc)
	require.Contains(t, bundle.DataRuleDataDoc, "kind: RuleData")
	require.Contains(t, bundle.DataRuleDataDoc, "name: coreruleset-data")
	require.Contains(t, bundle.DataRuleDataDoc, "foo.data:")
	require.Contains(t, bundle.RuleSetDoc, "- name: coreruleset-data")
}

func TestBuild_rejectsInvalidDataFileKeyFromFilename(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "bad name.data"), []byte("x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "ok.conf"), []byte(`SecRule ARGS "@rx a" "id:1,pass"`+"\n"), 0o644))
	scan, err := Scan(tmp)
	require.NoError(t, err)
	ver := mustParseCRSVersion(t, "4.0.0")
	_, err = Build(Options{
		RulesDir:       tmp,
		Version:        "4.0.0",
		RuleSetName:    "rs",
		DataSourceName: "coreruleset-data",
	}, scan, ver)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid RuleData files key")
}

func TestBuild_ignoreRuleIDs_dropsRuleFromExtraRuleSource(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "two.conf")
	err := os.WriteFile(path, []byte(
		`SecRule ARGS "@rx a" "id:100,phase:2,pass,nolog"
SecRule ARGS "@rx b" "id:200,phase:2,pass,nolog"
`), 0o644)
	require.NoError(t, err)

	ver := mustParseCRSVersion(t, "4.24.1")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:       tmp,
		Version:        "4.24.1",
		RuleSetName:    "default-ruleset",
		DataSourceName: "coreruleset-data",
		IgnoreRuleIDs:  map[string]struct{}{"100": {}},
	}, scan, ver)
	require.NoError(t, err)

	require.Len(t, bundle.ExtraRuleSources, 1)
	require.Equal(t, "two", bundle.ExtraRuleSources[0].Name)
	require.NotContains(t, bundle.ExtraRuleSources[0].Doc, "id:100,")
	require.Contains(t, bundle.ExtraRuleSources[0].Doc, "id:200,")
}

func TestBuild_rejectsInvalidPrefixedRuleSourceName(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "x.conf")
	require.NoError(t, os.WriteFile(path, []byte(`SecRule ARGS "@rx a" "id:1,pass"`+"\n"), 0o644))
	scan, err := Scan(tmp)
	require.NoError(t, err)
	ver := mustParseCRSVersion(t, "4.0.0")
	_, err = Build(Options{
		RulesDir:       tmp,
		Version:        "4.0.0",
		RuleSetName:    "rs",
		DataSourceName: "ds",
		NamePrefix:     "bad_",
	}, scan, ver)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid RuleSource name")
}

func TestBuild_rejectsRuleSourceNameTooLongAfterPrefix(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "a.conf")
	require.NoError(t, os.WriteFile(path, []byte(`SecRule ARGS "@rx a" "id:1,pass"`+"\n"), 0o644))
	scan, err := Scan(tmp)
	require.NoError(t, err)
	ver := mustParseCRSVersion(t, "4.0.0")
	_, err = Build(Options{
		RulesDir:       tmp,
		Version:        "4.0.0",
		RuleSetName:    "rs",
		DataSourceName: "ds",
		NamePrefix:     strings.Repeat("a", 253),
	}, scan, ver)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid RuleSource name")
}

func mustParseCRSVersion(t *testing.T, v string) CRSVersion {
	t.Helper()
	ver, err := ParseCRSVersion(v)
	require.NoError(t, err)
	return ver
}

func TestBuild_excludesUnsupportedRulesWithWASMProfile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "unsup.conf")
	err := os.WriteFile(path, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"+
			"SecRule ARGS \"@rx b\" \"id:42,phase:2,pass,nolog\"\n"), 0o644)
	require.NoError(t, err)

	ver := mustParseCRSVersion(t, "4.24.1")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:               tmp,
		Version:                "4.24.1",
		RuleSetName:            "rs",
		DataSourceName:         "ds",
		IgnoreUnsupportedRules: "wasm",
	}, scan, ver)
	require.NoError(t, err)

	require.Len(t, bundle.ExtraRuleSources, 1)
	require.NotContains(t, bundle.ExtraRuleSources[0].Doc, "id:922110,")
	require.Contains(t, bundle.ExtraRuleSources[0].Doc, "id:42,")
}

func TestBuild_includesUnsupportedRulesWhenProfileNone(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "unsup.conf")
	err := os.WriteFile(path, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"+
			"SecRule ARGS \"@rx b\" \"id:42,phase:2,pass,nolog\"\n"), 0o644)
	require.NoError(t, err)

	ver := mustParseCRSVersion(t, "4.24.1")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:               tmp,
		Version:                "4.24.1",
		RuleSetName:            "rs",
		DataSourceName:         "ds",
		IgnoreUnsupportedRules: "none",
	}, scan, ver)
	require.NoError(t, err)

	require.Len(t, bundle.ExtraRuleSources, 1)
	require.Contains(t, bundle.ExtraRuleSources[0].Doc, "id:922110,")
	require.Contains(t, bundle.ExtraRuleSources[0].Doc, "id:42,")
}

func TestBuild_profileMergesWithUserIgnoreIDs(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "mixed.conf")
	err := os.WriteFile(path, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"+
			"SecRule ARGS \"@rx b\" \"id:42,phase:2,pass,nolog\"\n"+
			"SecRule ARGS \"@rx c\" \"id:99,phase:2,pass,nolog\"\n"), 0o644)
	require.NoError(t, err)

	ver := mustParseCRSVersion(t, "4.24.1")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:               tmp,
		Version:                "4.24.1",
		RuleSetName:            "rs",
		DataSourceName:         "ds",
		IgnoreRuleIDs:          map[string]struct{}{"42": {}},
		IgnoreUnsupportedRules: "wasm",
	}, scan, ver)
	require.NoError(t, err)

	require.Len(t, bundle.ExtraRuleSources, 1)
	require.NotContains(t, bundle.ExtraRuleSources[0].Doc, "id:922110,")
	require.NotContains(t, bundle.ExtraRuleSources[0].Doc, "id:42,")
	require.Contains(t, bundle.ExtraRuleSources[0].Doc, "id:99,")
}

func TestBuild_confResultWarnsWhenIgnoringPMFromFile(t *testing.T) {
	tmp := t.TempDir()
	const name = "pm.conf"
	path := filepath.Join(tmp, name)
	err := os.WriteFile(path, []byte(`SecRule ARGS "@rx x" "id:1,phase:2,pass,nolog,chain"
SecRule ARGS "@pmFromFile foo.data" "id:2,phase:2,pass,nolog"
`+"\n"), 0o644)
	require.NoError(t, err)

	ver := mustParseCRSVersion(t, "4.0.0")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:         tmp,
		Version:          "4.0.0",
		RuleSetName:      "rs",
		DataSourceName:   "ds",
		IgnorePMFromFile: true,
	}, scan, ver)
	require.NoError(t, err)

	var found bool
	for _, w := range bundle.ConfFileResults[0].Warns {
		if strings.Contains(w, "SecRule chain") && strings.Contains(w, "@pmFromFile") {
			found = true
			break
		}
	}
	require.True(t, found, "expected chain warn when IgnorePMFromFile drops a chained @pmFromFile rule")
}

func TestBuild_unknownProfileSkipsRegistryMerge(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "future.conf")
	require.NoError(t, os.WriteFile(path, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"), 0o644))

	ver := mustParseCRSVersion(t, "4.24.1")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:               tmp,
		Version:                "4.24.1",
		RuleSetName:            "rs",
		DataSourceName:         "ds",
		IgnoreUnsupportedRules: "ext_proc",
	}, scan, ver)
	require.NoError(t, err)

	require.Len(t, bundle.ExtraRuleSources, 1)
	require.Contains(t, bundle.ExtraRuleSources[0].Doc, "id:922110,", "unknown profile must not merge WASM registry IDs")
}

func TestBuild_emptyIgnoreUnsupportedRulesSkipsRegistryMerge(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "libdefault.conf")
	require.NoError(t, os.WriteFile(path, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"), 0o644))

	ver := mustParseCRSVersion(t, "4.24.1")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:       tmp,
		Version:        "4.24.1",
		RuleSetName:    "rs",
		DataSourceName: "ds",
	}, scan, ver)
	require.NoError(t, err)

	require.Len(t, bundle.ExtraRuleSources, 1)
	require.Contains(t, bundle.ExtraRuleSources[0].Doc, "id:922110,", "library zero Options must not imply wasm profile (embedders set IgnoreUnsupportedRules explicitly)")
}

func TestBuild_redundantTierWASMUnsupportedStripped(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "redundant.conf")
	require.NoError(t, os.WriteFile(path, []byte(
		"SecRule ARGS \"@rx a\" \"id:920100,phase:2,pass,nolog\"\n"+
			"SecRule ARGS \"@rx b\" \"id:42,phase:2,pass,nolog\"\n"), 0o644))

	ver := mustParseCRSVersion(t, "4.24.1")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:               tmp,
		Version:                "4.24.1",
		RuleSetName:            "rs",
		DataSourceName:         "ds",
		IgnoreUnsupportedRules: "wasm",
	}, scan, ver)
	require.NoError(t, err)

	require.Len(t, bundle.ExtraRuleSources, 1)
	require.NotContains(t, bundle.ExtraRuleSources[0].Doc, "id:920100,")
	require.Contains(t, bundle.ExtraRuleSources[0].Doc, "id:42,")
}

func TestBuild_wasmProfileTrimmedAndCaseInsensitive(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "case.conf")
	require.NoError(t, os.WriteFile(path, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"+
			"SecRule ARGS \"@rx b\" \"id:42,phase:2,pass,nolog\"\n"), 0o644))

	ver := mustParseCRSVersion(t, "4.24.1")
	scan, err := Scan(tmp)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:               tmp,
		Version:                "4.24.1",
		RuleSetName:            "rs",
		DataSourceName:         "ds",
		IgnoreUnsupportedRules: "  WaSm  ",
	}, scan, ver)
	require.NoError(t, err)

	require.Len(t, bundle.ExtraRuleSources, 1)
	require.NotContains(t, bundle.ExtraRuleSources[0].Doc, "id:922110,")
	require.Contains(t, bundle.ExtraRuleSources[0].Doc, "id:42,")
}
