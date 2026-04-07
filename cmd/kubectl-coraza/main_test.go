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
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	// Output should contain a RuleSet and ConfigMap documents.
	assert.Contains(t, stdout.String(), "kind: ConfigMap")
	assert.Contains(t, stdout.String(), "kind: RuleSet")
}

func TestGenCRS_ignoreRules(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-rules", "100"})

	err := cmd.Execute()
	require.NoError(t, err)

	// The rule ID 100 from simple.conf should be dropped from the output.
	assert.NotContains(t, stdout.String(), "id:100,")
}

func TestGenCRS_ignoreRulesMultiple(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-rules", " 200 , 100 , "})

	err := cmd.Execute()
	require.NoError(t, err)

	// Both IDs should be absent from the generated output.
	// Rule ID 100 exists in the fixture; 200 does not but the CSV is still parsed.
	assert.NotContains(t, stdout.String(), "id:100,")
}

func TestGenCRS_ignoreRulesEmpty(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, _, stderr := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--ignore-rules", " , , "})

	err := cmd.Execute()
	require.NoError(t, err)

	// Empty CSV should not print the ignoring message.
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

	// Dry-run still writes manifests to stdout.
	assert.NotEmpty(t, stdout.String())
	assert.Contains(t, stdout.String(), "kind: RuleSet")
}

func TestGenCRS_dryRunCaseInsensitive(t *testing.T) {
	dir := testdataDir(t, "minimal")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.24.1", "--dry-run", " CLIENT "})

	err := cmd.Execute()
	require.NoError(t, err)

	// Case-insensitive "CLIENT" should also produce valid output.
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

	// The RuleSet document should contain the custom name in its metadata.
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
// genCRS Tests — Data Secret
// -----------------------------------------------------------------------------

func TestGenCRS_withDataSecret(t *testing.T) {
	dir := testdataDir(t, "withdata")
	cmd, stdout, _ := newTestCommand(t)
	cmd.SetArgs([]string{"generate", "coreruleset", "--rules-dir", dir, "--version", "4.0.0"})

	err := cmd.Execute()
	require.NoError(t, err)

	assert.Contains(t, stdout.String(), "kind: Secret")
	assert.Contains(t, stdout.String(), "kind: RuleSet")
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

// -----------------------------------------------------------------------------
// Test Helpers
// -----------------------------------------------------------------------------

// testdataDir returns the path to the shared corerulesetgen testdata fixtures.
func testdataDir(t *testing.T, fixture string) string {
	t.Helper()
	dir := filepath.Join("..", "..", "tools", "corerulesetgen", "testdata", fixture, "rules")
	_, err := os.Stat(dir)
	require.NoError(t, err, "fixture directory must exist: %s", dir)
	return dir
}

// newTestCommand builds a fresh cobra command tree identical to the real one,
// with stdout/stderr captured for assertions.
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
	flags.String("data-secret-name", "coreruleset-data", "")
	flags.String("name-prefix", "", "")
	flags.String("name-suffix", "", "")
	flags.String("dry-run", "", "")
	flags.Bool("skip-size-check", false, "")

	root.AddCommand(generate)
	generate.AddCommand(coreruleset)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	return root, &stdout, &stderr
}
