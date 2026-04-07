package corerulesetgen

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerate_minimalFixture(t *testing.T) {
	dir := filepath.Join("testdata", "minimal")
	var out bytes.Buffer
	var errBuf bytes.Buffer
	_, err := Generate(&out, Options{
		RulesDir: filepath.Join(dir, "rules"),
		Version:  "4.24.1",
		Stderr:   &errBuf,
	})
	require.NoError(t, err)

	want, err := os.ReadFile(filepath.Join(dir, "golden.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(want), out.String(), "update golden with: go test ./tools/corerulesetgen -run TestGenerate_minimalFixture")
}

func TestGenerate_withDataSource(t *testing.T) {
	dir := filepath.Join("testdata", "withdata")
	var out bytes.Buffer
	var errBuf bytes.Buffer
	_, err := Generate(&out, Options{
		RulesDir: filepath.Join(dir, "rules"),
		Version:  "4.0.0",
		Stderr:   &errBuf,
	})
	require.NoError(t, err)

	want, err := os.ReadFile(filepath.Join(dir, "golden.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(want), out.String())
}

func TestBuildPipeline_minimalFixture(t *testing.T) {
	dir := filepath.Join("testdata", "minimal")
	rulesPath := filepath.Join(dir, "rules")
	ver, err := ParseCRSVersion("4.24.1")
	require.NoError(t, err)
	scan, err := Scan(rulesPath)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:       rulesPath,
		Version:        "4.24.1",
		RuleSetName:    "default-ruleset",
		DataSourceName: "coreruleset-data",
	}, scan, ver)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, WriteManifests(&out, bundle))

	want, err := os.ReadFile(filepath.Join(dir, "golden.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(want), out.String())
}

func TestBuildPipeline_withDataSource(t *testing.T) {
	dir := filepath.Join("testdata", "withdata")
	rulesPath := filepath.Join(dir, "rules")
	ver, err := ParseCRSVersion("4.0.0")
	require.NoError(t, err)
	scan, err := Scan(rulesPath)
	require.NoError(t, err)

	bundle, err := Build(Options{
		RulesDir:       rulesPath,
		Version:        "4.0.0",
		RuleSetName:    "default-ruleset",
		DataSourceName: "coreruleset-data",
	}, scan, ver)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, WriteManifests(&out, bundle))

	want, err := os.ReadFile(filepath.Join(dir, "golden.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(want), out.String())
}

func TestGenerate_ignoreRuleID(t *testing.T) {
	dir := filepath.Join("testdata", "minimal", "rules")
	var out bytes.Buffer
	_, err := Generate(&out, Options{
		RulesDir:      dir,
		Version:       "4.24.1",
		IgnoreRuleIDs: map[string]struct{}{"100": {}},
		Stderr:        io.Discard,
	})
	require.NoError(t, err)
	require.NotContains(t, out.String(), "id:100,")
}

func TestParseCRSVersion(t *testing.T) {
	ver, err := ParseCRSVersion("v4.24.1")
	require.NoError(t, err)
	require.Equal(t, "4.24.1", ver.Normalized)
	require.Equal(t, "4241", ver.Setup)

	_, err = ParseCRSVersion("not-a-version")
	require.Error(t, err)
}
