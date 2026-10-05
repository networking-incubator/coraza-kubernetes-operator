package corerulesetgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitIntoRules_multilineTrailingSpaceAfterBackslash(t *testing.T) {
	// Backslash continuation with trailing spaces before newline (common in CRS).
	line1 := `SecRule ARGS "@rx x" "id:1,pass" \   `
	content := line1 + "\n" + `"cont" "id:2,pass"`
	blocks := splitIntoRules(content)
	require.Len(t, blocks, 1)
	require.Contains(t, blocks[0], "id:1")
	require.Contains(t, blocks[0], "cont")
}

func TestChainSecRuleGroups(t *testing.T) {
	t.Run("standalone", func(t *testing.T) {
		blocks := splitIntoRules(`SecRule A "op" "id:1,pass"
SecRule B "op" "id:2,pass"`)
		g := chainSecRuleGroups(blocks)
		require.Len(t, g, 2)
		require.Equal(t, []int{0}, g[0])
		require.Equal(t, []int{1}, g[1])
	})

	t.Run("two_rule_chain", func(t *testing.T) {
		blocks := splitIntoRules(`SecRule ARGS "@rx x" "id:1,phase:2,pass,chain"
SecRule ARGS "@pmFromFile f" "id:2,phase:2,pass"`)
		g := chainSecRuleGroups(blocks)
		require.Len(t, g, 1)
		require.Equal(t, []int{0, 1}, g[0])
	})

	t.Run("comment_between_chained_rules", func(t *testing.T) {
		blocks := splitIntoRules(`SecRule ARGS "@rx x" "id:1,chain"
# comment
SecRule ARGS "@rx y" "id:2,pass"`)
		g := chainSecRuleGroups(blocks)
		require.Len(t, g, 1)
		require.Equal(t, []int{0, 2}, g[0])
	})
}

func TestProcessFileContent_dropsFullChainWhenPMIgnored(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "x.conf")
	content := `SecRule ARGS "@rx x" "id:1,phase:2,pass,nolog,chain"
SecRule ARGS "@pmFromFile foo.data" "id:2,phase:2,pass,nolog"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	out, warns, err := processFileContent(path, nil, nil, true)
	require.NoError(t, err)
	require.NotContains(t, out, "chain")
	require.NotContains(t, out, "id:1")
	require.NotContains(t, out, "id:2")
	require.True(t, strings.Contains(strings.Join(warns, ""), "SecRule chain"))
}

func TestProcessFileContent_warnAutoIgnoreVsUserIgnore(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "x.conf")
	content := `SecRule ARGS "@rx a" "id:922110,phase:2,pass,nolog"`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	_, warns, err := processFileContent(path, map[string]struct{}{"922110": {}}, map[string]struct{}{"922110": {}}, false)
	require.NoError(t, err)
	joined := strings.Join(warns, "")
	require.Contains(t, joined, "--ignore-unsupported-rules")
	require.Contains(t, joined, "profile")

	_, warns2, err := processFileContent(path, map[string]struct{}{"922110": {}}, nil, false)
	require.NoError(t, err)
	joined2 := strings.Join(warns2, "")
	require.Contains(t, joined2, "Rule ID in ignore list")
	require.NotContains(t, joined2, "--ignore-unsupported-rules")
}

func TestProcessFileContent_dropsFullChainWhenIDIgnored(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "x.conf")
	content := `SecRule ARGS "@rx x" "id:10,phase:2,pass,chain"
SecRule ARGS "@rx y" "id:20,phase:2,pass"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	out, warns, err := processFileContent(path, map[string]struct{}{"20": {}}, nil, false)
	require.NoError(t, err)
	require.NotContains(t, out, "id:10")
	require.NotContains(t, out, "id:20")
	require.True(t, strings.Contains(strings.Join(warns, ""), "SecRule chain"))
}

// Regression: CRS writes chained rules with the continuation on its own
// indented line (",\" then "    chain\"") and the chained SecRule indented too.
// Both forms were invisible to the splitter and the chain detector, so ignoring
// a chain starter left its children behind as standalone rules. For 920420 that
// orphan scored every request carrying a Content-Type and 403'd all POSTs.
func TestProcessFileContent_dropsIndentedCrsStyleChain(t *testing.T) {
	content := `SecRule REQUEST_HEADERS:Content-Type "@rx ^[^;\s]+" \
    "id:920420,\
    phase:1,\
    block,\
    capture,\
    t:none,\
    msg:'Request content type is not allowed by policy',\
    severity:'CRITICAL',\
    setvar:'tx.content_type=|%{tx.0}|',\
    chain"
    SecRule TX:content_type "!@within %{tx.allowed_request_content_type}" \
        "t:lowercase,\
        setvar:'tx.inbound_anomaly_score_pl1=+%{tx.critical_anomaly_score}'"

SecRule ARGS "@rx keep-me" "id:999999,phase:2,pass"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "REQUEST-920-PROTOCOL-ENFORCEMENT.conf")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	out, _, err := processFileContent(path, map[string]struct{}{"920420": {}}, nil, false)
	require.NoError(t, err)

	require.NotContains(t, out, "920420", "chain starter must be dropped")
	require.NotContains(t, out, "TX:content_type",
		"chained child must be dropped with its starter, not left orphaned")
	require.Contains(t, out, "999999", "unrelated rules must survive")
}

func TestSecRuleHasChainAction_continuationBeforeChain(t *testing.T) {
	// ",\" + newline + indentation + `chain"` is the standard CRS layout.
	block := "SecRule A \"@rx x\" \\\n    \"id:1,\\\n    phase:1,\\\n    chain\""
	require.True(t, secRuleHasChainAction(block))
}
