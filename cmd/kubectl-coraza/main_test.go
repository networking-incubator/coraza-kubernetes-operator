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

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// -----------------------------------------------------------------------------
// genCRS Tests
// -----------------------------------------------------------------------------

func TestGenCRS_minimalFixture(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1"})

	err := cmd.Execute()
	require.NoError(t, err)
	require.NotEmpty(t, stdout.String())

	assert.Contains(t, stdout.String(), "kind: RuleSource")
	assert.Contains(t, stdout.String(), "kind: RuleSet")
}

func TestGenCRS_ignoreRules(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-rules", "100"})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.NotContains(t, stdout.String(), "id:100,")
}

func TestGenCRS_ignoreRulesMultiple(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-rules", " 200 , 100 , "})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.NotContains(t, stdout.String(), "id:100,")
}

func TestGenCRS_ignoreRulesEmpty(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, _, stderr := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-rules", " , , "})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.NotContains(t, stderr.String(), "Ignoring rule IDs")
}

func TestGenCRS_ignoreRulesWritesToCmdStderr(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, _, stderr := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-rules", "100"})

	err := cmd.Execute()
	require.NoError(t, err)

	// The "Ignoring rule IDs" message must be routed through cmd.ErrOrStderr()
	// so it appears in the captured stderr buffer, not lost to os.Stderr.
	assert.Contains(t, stderr.String(), "Ignoring rule IDs")
	assert.Contains(t, stderr.String(), "100")
}

func TestGenCRS_dryRun(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--dry-run", "client"})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.NotEmpty(t, stdout.String())
	assert.Contains(t, stdout.String(), "kind: RuleSet")
}

func TestGenCRS_dryRunCaseInsensitive(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--dry-run", " CLIENT "})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.NotEmpty(t, stdout.String())
	assert.Contains(t, stdout.String(), "kind: RuleSet")
}

func TestGenCRS_namespace(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "-n", "waf-system"})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "namespace: waf-system")
}

func TestGenCRS_customRulesetName(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ruleset-name", "my-ruleset"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := stdout.String()
	assert.Contains(t, output, "name: my-ruleset")
	assert.Contains(t, output, "kind: RuleSet")
}

func TestGenCRS_invalidRulesDir(t *testing.T) {
	cmd, _, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", "/nonexistent/path", "--version", "4.24.1"})

	err := cmd.Execute()
	require.Error(t, err)
}

func TestGenCRS_invalidVersion(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, _, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "not-a-version"})

	err := cmd.Execute()
	require.Error(t, err)
}

func TestGenCRS_invalidRulesetName(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, _, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ruleset-name", "INVALID NAME!"})

	err := cmd.Execute()
	require.Error(t, err)
}

// -----------------------------------------------------------------------------
// genCRS Tests — Data RuleSource
// -----------------------------------------------------------------------------

func TestGenCRS_withDataSource(t *testing.T) {
	dir := testdataDir(t, "withdata")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.0.0"})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.Contains(t, stdout.String(), "kind: RuleData")
	assert.Contains(t, stdout.String(), "kind: RuleSet")
}

func TestGenCRS_excludesUnsupportedByDefault(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "unsup.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"+
			"SecRule ARGS \"@rx b\" \"id:42,phase:2,pass,nolog\"\n"), 0o644))

	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1"})

	require.NoError(t, cmd.Execute())
	assert.NotContains(t, stdout.String(), "id:922110,")
	assert.Contains(t, stdout.String(), "id:42,")
}

func TestGenCRS_ignoreUnsupportedRulesNone(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "unsup.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"+
			"SecRule ARGS \"@rx b\" \"id:42,phase:2,pass,nolog\"\n"), 0o644))

	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-unsupported-rules=none"})

	require.NoError(t, cmd.Execute())
	assert.Contains(t, stdout.String(), "id:922110,")
	assert.Contains(t, stdout.String(), "id:42,")
}

func TestGenCRS_ignoreUnsupportedRulesExplicitWASM(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "unsup.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"+
			"SecRule ARGS \"@rx b\" \"id:42,phase:2,pass,nolog\"\n"), 0o644))

	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-unsupported-rules=wasm"})

	require.NoError(t, cmd.Execute())
	assert.NotContains(t, stdout.String(), "id:922110,")
	assert.Contains(t, stdout.String(), "id:42,")
}

func TestGenCRS_unknownIgnoreUnsupportedProfileKeepsRegistryRule(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "unsup.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"), 0o644))

	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-unsupported-rules=ext_proc"})

	require.NoError(t, cmd.Execute())
	assert.Contains(t, stdout.String(), "id:922110,", "unknown profile should not apply WASM registry until implemented")
}

func TestGenCRS_ignoreUnsupportedRulesWASMAllowsMixedCase(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "unsup.conf")
	require.NoError(t, os.WriteFile(confPath, []byte(
		"SecRule ARGS \"@rx a\" \"id:922110,phase:2,pass,nolog\"\n"), 0o644))

	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-unsupported-rules=WaSm"})

	require.NoError(t, cmd.Execute())
	assert.NotContains(t, stdout.String(), "id:922110,")
}

func TestGenCRS_missingRequiredFlags(t *testing.T) {
	cmd, _, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset"})

	err := cmd.Execute()
	require.Error(t, err)
}

func TestGenCRS_versionWithPrefix(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "v4.24.1"})

	err := cmd.Execute()
	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "kind: RuleSet")
}

func TestGenCRS_generatedManifestsParseAsYAML(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		version string
	}{
		{"minimal", "minimal", "4.24.1"},
		{"withRuleData", "withdata", "4.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := testdataDir(t, tc.fixture)
			cmd, stdout, _ := newTestCommand(t)
			cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", tc.version})

			err := cmd.Execute()
			require.NoError(t, err)

			n := requireMultiDocYAMLParses(t, stdout.String())
			require.Greater(t, n, 0, "expected at least one YAML document")
		})
	}
}

// -----------------------------------------------------------------------------
// Test Helpers
// -----------------------------------------------------------------------------

func testdataDir(t *testing.T, fixture string) string {
	t.Helper()
	dir := filepath.Join("..", "..", "tools", "corerulesetgen", "testdata", fixture, "rules")
	_, err := os.Stat(dir)
	require.NoError(t, err, "fixture directory must exist: %s", dir)
	return dir
}

// requireMultiDocYAMLParses decodes every document in a multi-doc YAML stream (kubectl-style --- separators).
// It returns the number of documents successfully decoded; use this to catch invalid indentation and other parse errors.
func requireMultiDocYAMLParses(t *testing.T, s string) int {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(s))
	n := 0
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "YAML document %d", n+1)
		n++
	}
	return n
}

func newTestCommand(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := &cobra.Command{
		Use: "kubectl-coraza",
	}
	root.CompletionOptions.DisableDefaultCmd = true

	generate := &cobra.Command{
		Use: "generate",
	}
	coreruleset := &cobra.Command{
		Use:  "coreruleset",
		RunE: genCRS,
	}

	flags := coreruleset.Flags()
	flags.String("rules-dir", "", "")
	_ = coreruleset.MarkFlagRequired("rules-dir")
	flags.String("version", "", "")
	_ = coreruleset.MarkFlagRequired("version")
	flags.String("ignore-rules", "", "")
	flags.Bool("ignore-pmFromFile", false, "")
	flags.Bool("include-test-rule", false, "")
	flags.String("ruleset-name", "default-ruleset", "")
	flags.StringP("namespace", "n", "", "")
	flags.String("data-source-name", "coreruleset-data", "")
	flags.String("name-prefix", "", "")
	flags.String("name-suffix", "", "")
	flags.String("dry-run", "", "")
	flags.Bool("skip-size-check", false, "")
	flags.String("ignore-unsupported-rules", "wasm", "")

	root.AddCommand(generate)
	generate.AddCommand(coreruleset)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	return root, &stdout, &stderr
}
