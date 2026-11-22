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
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/corazawaf/coraza/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckGatewayParity covers rule text Coraza and envoy-coraza read
// differently. The RuleSource check, which runs before Coraza compiles tenant
// text, rejects only directive names and backtick block shapes
// (ruleSourceRejects); unless corazaRejects is set, Coraza alone accepts the
// other rule text, so the RuleSet check is what rejects it.
func TestCheckGatewayParity(t *testing.T) {
	const rblRule = `SecRule REMOTE_ADDR "@rbl sbl.spamhaus.org" "id:2,phase:1,deny"`
	const twoRules = "SecRule ARGS \"@rx a\" \"id:1,phase:1,deny\"\nSecRule ARGS \"@rx b\" \"id:2,phase:1,deny\"\n"
	const argumentQuotes = "arguments in double quotes must not be empty or start or end with another double quote"
	const actionQuotes = "an action list must be enclosed in one pair of double quotes, or in none"
	const lastArgument = "the last argument must be one field, enclosed in one pair of double quotes or in none"
	longComment := "# " + strings.Repeat("a", maxLineBytes)
	included := func(text string) map[string][]byte {
		return map[string][]byte{"extra.conf": []byte(text)}
	}

	tests := []struct {
		name              string
		rules             string
		dataFiles         map[string][]byte
		wantErr           string
		ruleSourceRejects bool
		corazaRejects     bool
	}{
		{
			name:    "space before an operator",
			rules:   `SecRule REMOTE_ADDR " @rbl sbl.spamhaus.org" "id:1,phase:1,deny"`,
			wantErr: errLeadingSpaceOperator.Error(),
		},
		{
			name:    "tab before a negated operator",
			rules:   "SecRule ARGS \"\t!@rx a\" \"id:1,phase:1,deny\"",
			wantErr: errLeadingSpaceOperator.Error(),
		},
		{
			name:    "space before a negated pattern",
			rules:   `SecRule ARGS " !attack" "id:1,phase:1,deny"`,
			wantErr: errLeadingSpaceOperator.Error(),
		},
		{
			name:    "space before an operator in a chained rule",
			rules:   "SecRule REQUEST_URI \"@beginsWith /\" \"id:1,phase:1,deny,chain\"\n  SecRule ARGS \" @contains a\"",
			wantErr: errLeadingSpaceOperator.Error(),
		},
		{
			name:      "space before an operator in an included RuleData file",
			rules:     "Include extra.conf",
			dataFiles: included(`SecRule ARGS " @contains a" "id:1,phase:1,deny"`),
			wantErr:   "included RuleData file extra.conf: " + errLeadingSpaceOperator.Error(),
		},
		{
			name:    "rule compilation errors ignored",
			rules:   "SecIgnoreRuleCompilationErrors On\n" + rblRule,
			wantErr: errIgnoreCompilationErrors.Error(),
		},
		{
			name:    "rule compilation errors ignored, quoted",
			rules:   "secignorerulecompilationerrors \"on\"\n" + rblRule,
			wantErr: errIgnoreCompilationErrors.Error(),
		},
		{
			name:    "a long line stops Coraza reading the rest of the rule text",
			rules:   longComment + "\n" + rblRule,
			wantErr: "line 1 is 65538 bytes: lines of 65536 bytes or more are not supported, because Coraza stops reading rule text there; split it with a trailing backslash",
		},
		{
			name:      "a long line in an included RuleData file",
			rules:     "Include extra.conf",
			dataFiles: included("SecRuleEngine On\n" + longComment + "\n" + rblRule),
			wantErr:   "included RuleData file extra.conf: line 2 is 65538 bytes",
		},
		{
			name:              "non-ASCII directive name",
			rules:             "SecRuleEngİne Off",
			wantErr:           `directive "SecRuleEngİne" is not supported: directive names are ASCII, and Coraza and envoy-coraza lowercase other letters differently`,
			ruleSourceRejects: true,
		},
		{
			name:              "long non-ASCII directive name",
			rules:             "İ" + strings.Repeat("a", 40<<10) + " On",
			wantErr:           `directive "İ` + strings.Repeat("a", 62) + `"... is not supported`,
			ruleSourceRejects: true,
		},
		{
			name:              "SecAction backtick block containing a ctl action",
			rules:             "SecAction \"id:1,phase:1,pass,nolog,tag:`\n`,ctl:ruleEngine=Off\"",
			wantErr:           errBacktickBlock.Error(),
			ruleSourceRejects: true,
		},
		{
			name:              "SecRule backtick block containing an operator argument",
			rules:             "SecRule ARGS \"@pm a`\n` b\" \"id:1,phase:1,deny\"",
			wantErr:           errBacktickBlock.Error(),
			ruleSourceRejects: true,
		},
		{
			name:              "SecRule backtick block containing whitespace before the operator",
			rules:             "SecRule ARGS \" @rbl x`\n`\" \"id:1,phase:1,deny\"",
			wantErr:           errBacktickBlock.Error(),
			ruleSourceRejects: true,
		},
		{
			name:              "SecDataset block closed with text after the backtick",
			rules:             "SecDataset words `\nattack\n` SecRuleEngine Off",
			wantErr:           errBacktickClose.Error(),
			ruleSourceRejects: true,
		},
		{
			name:              "SecDataset block closed with a continuation",
			rules:             "SecDataset words `\nattack\n`\\\nSecRuleEngine Off",
			wantErr:           errBacktickClose.Error(),
			ruleSourceRejects: true,
		},
		{
			name:    "action update with an opening quote only",
			rules:   twoRules + `SecRuleUpdateActionById 1 "ctl:ruleEngine=Off`,
			wantErr: "an action list must be enclosed in one pair of double quotes, or in none",
		},
		{
			name:    "action update with an opening quote only, after a range",
			rules:   twoRules + `SecRuleUpdateActionById 1-2 "ctl:ruleEngine=Off`,
			wantErr: "an action list must be enclosed in one pair of double quotes, or in none",
		},
		{
			name:    "wholly quoted action update with a doubled closing quote",
			rules:   twoRules + `SecRuleUpdateActionById "1 "ctl:ruleEngine=Off""`,
			wantErr: argumentQuotes,
		},
		{
			name:    "action update with a quote inside the action list",
			rules:   twoRules + `SecRuleUpdateActionById 1 "msg:'a"ctl:ruleEngine=Off'"`,
			wantErr: "an action list must be enclosed in one pair of double quotes, or in none",
		},
		{
			name:    "action update with an escaped quote inside the action list",
			rules:   twoRules + `SecRuleUpdateActionById 1 "pass,msg:'a\"b'"`,
			wantErr: "an action list must be enclosed in one pair of double quotes, or in none",
		},
		{name: "unquoted action update ending with an escaped quote", rules: twoRules + `SecRuleUpdateActionById 1 pass,msg:x\"`, wantErr: actionQuotes},
		{name: "unquoted action update for a range ending with an escaped quote", rules: twoRules + `SecRuleUpdateActionById 1-2 nolog,msg:x\"`, wantErr: actionQuotes},
		{name: "unquoted action update with a ctl action ending with an escaped quote", rules: twoRules + `SecRuleUpdateActionById 1 pass,ctl:ruleRemoveByTag=x\"`, wantErr: actionQuotes},
		{name: "unquoted target update ending with a quote", rules: twoRules + `SecRuleUpdateTargetById 1 !ARGS:x"`, wantErr: lastArgument},
		{name: "target update with a doubled closing quote", rules: twoRules + `SecRuleUpdateTargetById 1 "!ARGS:x""`, wantErr: lastArgument},
		{name: "unquoted target update by tag ending with a quote", rules: twoRules + `SecRuleUpdateTargetByTag t !ARGS:x"`, wantErr: lastArgument},
		{name: "target update with an escaped quote inside the variables", rules: twoRules + `SecRuleUpdateTargetById 1 "!ARGS:a\"b"`, wantErr: lastArgument},
		{
			name:          "SecAction with an opening quote only",
			rules:         `SecAction "id:9,phase:1,pass,ctl:ruleEngine=Off`,
			wantErr:       "an action list must be enclosed in one pair of double quotes, or in none",
			corazaRejects: true,
		},
		{
			name:    "SecDefaultAction with a closing quote only",
			rules:   `SecDefaultAction phase:1,log,pass"`,
			wantErr: "an action list must be enclosed in one pair of double quotes, or in none",
		},
		{name: "SecAction with a doubled closing quote", rules: `SecAction "id:9,phase:1,pass,nolog""`, wantErr: argumentQuotes},
		{name: "SecAction with a doubled opening quote", rules: `SecAction ""id:9,phase:1,pass,nolog"`, wantErr: argumentQuotes},
		{name: "SecDefaultAction with a doubled closing quote", rules: `SecDefaultAction "phase:1,log,pass""`, wantErr: argumentQuotes},
		{name: "SecDefaultAction in doubled quotes", rules: `SecDefaultAction ""phase:1,log,pass""`, wantErr: argumentQuotes},
		{name: "action update with a doubled closing quote", rules: twoRules + `SecRuleUpdateActionById "1 pass""`, wantErr: argumentQuotes},
		{name: "action update in doubled quotes", rules: twoRules + `SecRuleUpdateActionById ""1 pass""`, wantErr: argumentQuotes},
		{name: "setting in doubled quotes", rules: `SecRequestBodyLimit ""512""`, wantErr: argumentQuotes},
		{
			name:    "action update naming a missing rule after another",
			rules:   twoRules + `SecRuleUpdateActionById 1 999 "pass"`,
			wantErr: errUpdateActionIDs.Error(),
		},
		{
			name:    "action update naming two rules",
			rules:   twoRules + `SecRuleUpdateActionById 1 2 "pass"`,
			wantErr: errUpdateActionIDs.Error(),
		},
		{
			name:    "action update naming a one-rule range before another",
			rules:   twoRules + `SecRuleUpdateActionById 1-1 2 "pass"`,
			wantErr: errUpdateActionIDs.Error(),
		},
		{name: "unquoted action list ending with an escaped quote", rules: `SecAction id:1,phase:1,pass,nolog,msg:\"x\"`},
		{name: "quoted action list with escaped quotes", rules: `SecRule ARGS "@rx a" "id:2,phase:1,pass,msg:\"x\""`},
		{name: "SecRule action list ending with a backslash before its closing quote", rules: `SecRule ARGS "@rx a" "id:1,phase:1,pass,msg:x\"`},
		{name: "action update naming one rule", rules: twoRules + `SecRuleUpdateActionById 1 "pass"`},
		{name: "target update with quoted variables", rules: twoRules + `SecRuleUpdateTargetById 1 "!ARGS:foo"`},
		{name: "target update with unquoted variables", rules: twoRules + `SecRuleUpdateTargetById 1 !ARGS_NAMES:json.profile`},
		{name: "target update by a quoted tag", rules: twoRules + `SecRuleUpdateTargetByTag "attack-sqli" "!ARGS:foo"`},
		{name: "target update by an unquoted tag", rules: twoRules + `SecRuleUpdateTargetByTag attack-sqli !ARGS:foo`},
		{name: "action update naming a range and then a rule", rules: twoRules + `SecRuleUpdateActionById 1-2 1 "pass"`},
		{name: "action update naming two ranges", rules: twoRules + `SecRuleUpdateActionById 1-2 3-4 "pass"`},
		{name: "action update with an unquoted action list", rules: twoRules + `SecRuleUpdateActionById 1 pass`},
		{name: "space before a plain pattern", rules: `SecRule ARGS " attack" "id:1,phase:1,deny"`},
		{name: "operator text in the actions", rules: `SecRule ARGS "@rx a" "id:1,phase:1,deny,msg:' @rbl x'"`},
		{name: "operator text inside the pattern", rules: `SecRule ARGS "@rx \" @x" "id:1,phase:1,deny"`},
		{name: "operator text in SecAction", rules: `SecAction "id:1,phase:1,pass,msg:' @rbl x'"`},
		{name: "rule compilation errors reported", rules: "SecIgnoreRuleCompilationErrors Off"},
		{name: "longest line Coraza reads", rules: "# " + strings.Repeat("a", maxLineBytes-3)},
		{name: "non-ASCII dataset item", rules: "SecDataset words `\nİtem\n`\nSecRule ARGS \"@pmFromDataset words\" \"id:1,phase:1,deny\""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckGatewayParity(tt.rules, tt.dataFiles)
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.NoError(t, ValidateRuleSourceRules(tt.rules, "parity-rs", tt.dataFiles))
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
			switch {
			case tt.ruleSourceRejects:
				require.ErrorContains(t, ValidateRuleSourceRules(tt.rules, "parity-rs", tt.dataFiles), tt.wantErr)
			case tt.corazaRejects:
				require.Error(t, ValidateRuleSourceRules(tt.rules, "parity-rs", tt.dataFiles))
			default:
				require.NoError(t, ValidateRuleSourceRules(tt.rules, "parity-rs", tt.dataFiles), "Coraza alone accepts this rule text")
			}
		})
	}
}

// TestNonASCIIIncludeIsNeverCompiled covers an Include spelled with a non-ASCII
// letter: it is rejected as an unsupported directive name, so the included
// RuleData file is never compiled.
func TestNonASCIIIncludeIsNeverCompiled(t *testing.T) {
	target := filepath.Join(t.TempDir(), "debug.log")
	dataFiles := map[string][]byte{"extra.conf": []byte("SecDebugLog " + target + "\nSecDebugLogLevel 9\n")}
	rules := "İnclude extra.conf\nSecRule ARGS \"@rx a\" \"id:1,phase:1,deny\""

	require.NoError(t, ValidateIncludes(rules, dataFiles), "the Include guard does not read the name as Include")
	require.ErrorContains(t, CheckGatewayParity(rules, dataFiles), `directive "İnclude" is not supported`)
	require.ErrorContains(t, ValidateRuleSourceRules(rules, "parity-rs", dataFiles), `directive "İnclude" is not supported`)
	require.ErrorContains(t, ValidateRuleSourceRules(rules, "parity-rs", nil), `directive "İnclude" is not supported`)

	_, err := os.Stat(target)
	assert.ErrorIs(t, err, os.ErrNotExist, "the included file must never be compiled")

	// Coraza alone reads the same name as Include and compiles the file.
	compiled := filepath.Join(t.TempDir(), "debug.log")
	compiledData := map[string][]byte{"extra.conf": []byte("SecDebugLog " + compiled + "\n")}
	waf, err := coraza.NewWAF(BuildWAFConfig(rules, compiledData))
	require.NoError(t, err)
	CloseWAF(waf)
	_, err = os.Stat(compiled)
	require.NoError(t, err, "Coraza alone reads the non-ASCII name as Include")
}

// TestDatasetBlockClosingContinuation covers a SecDataset block whose closing
// line ends with a continuation: both engines read the following line as part
// of the SecDataset directive, so the checks that also run on accepted content
// do not read it as a directive, while new content is rejected.
func TestDatasetBlockClosingContinuation(t *testing.T) {
	for _, next := range []string{"SecRequestBodyLimit 99999999999", "Include /opt/rules/extra.conf"} {
		require.Error(t, errors.Join(ValidateIncludes(next, nil), ValidateAssembledBodyLimits(next, nil)), "the guards reject %s on its own", next)
		rules := "SecDataset words `\nattack\n`\\\n" + next + "\nSecRule ARGS \"@pmFromDataset words\" \"id:1,phase:1,deny\""
		require.NoError(t, ValidateIncludes(rules, nil), next)
		require.NoError(t, ValidateAssembledBodyLimits(rules, nil), next)
		require.NoError(t, ValidateCorazaRSIncludesForModule(rules, nil, "none"), next)
		require.NoError(t, ValidatePMFromFileData(rules, nil), next)
		require.ErrorIs(t, CheckGatewayParity(rules, nil), errBacktickClose, next)
	}
}

// joinWithoutBlocks joins continuation lines and drops comments without
// reading backtick blocks, as Coraza's parser does for rule text that has none.
func joinWithoutBlocks(rules string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(rules, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		continued := strings.HasSuffix(line, `\`)
		b.WriteString(strings.TrimSuffix(line, `\`))
		if !continued {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestJoinActiveRuleLinesWithoutBlocks keeps joinActiveRuleLines, which the
// Include and host guards read, unchanged for rule text without backtick
// blocks.
func TestJoinActiveRuleLinesWithoutBlocks(t *testing.T) {
	inputs := []string{
		"",
		"\n\n",
		"# comment only",
		"SecRuleEngine On",
		"SecRuleEngine On\n",
		"  SecRuleEngine On  \r\n# c\n\nSecRequestBodyAccess On\r\n",
		"SecRule ARGS \"@rx a\" \\\n  \"id:1,phase:1,deny\"",
		"SecRule ARGS \\\n# comment inside\n\n  \"@pmf a.data\" \\\n  \"id:1,phase:1,deny\"\n",
		"SecRule ARGS \"@rx a\" \"id:1,phase:1,deny,chain\"\n  SecRule ARGS \"@rx b\"\n",
		"Include extra.conf\nInclude \"crs/rules/*.conf\"\n",
		"SecAction \"id:1,phase:1,pass\" \\",
		"\\\n\\\nSecRuleEngine Off",
		"SecRule ARGS \"@rx a\\\\\" \"id:1\"\\\\\n",
		"SecRequestBodyLimit 1024\nSecRequestBodyAccess On\nSecAuditLogType Serial\n",
	}
	for _, input := range inputs {
		assert.Equal(t, joinWithoutBlocks(input), joinActiveRuleLines(input), "%q", input)
	}

	files, err := filepath.Glob("../../../tools/corerulesetgen/testdata/*/rules/*.conf")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, joinWithoutBlocks(string(data)), joinActiveRuleLines(string(data)), file)
	}
}

// TestBlockReopenedByAContinuedClosingLine covers a SecDataset closing line
// that continues into a line opening another block: both engines read that
// block as part of the first directive, so the directive after it is read as
// a directive again, and the RuleSource path rejects it before any compile.
func TestBlockReopenedByAContinuedClosingLine(t *testing.T) {
	debugLog := filepath.Join(t.TempDir(), "debug.log")
	rules := "SecDataset a `\nx\n`\\\nSecDataset b `\ny\n`\nSecDebugLog " + debugLog + "\n" +
		`SecRule ARGS "@rx a" "id:1,phase:1,deny"`

	require.ErrorContains(t, ValidateIncludes(rules, nil), "SecDebugLog is not allowed")
	require.ErrorIs(t, CheckDirectiveSyntax(rules, nil), errBacktickClose)
	require.ErrorIs(t, CheckGatewayParity(rules, nil), errBacktickClose)
	require.Error(t, ValidateRuleSourceRules(rules, "block-rs", nil))
	_, err := os.Stat(debugLog)
	assert.ErrorIs(t, err, os.ErrNotExist, "the rule text is never compiled")
}

// engineDirectives models how Coraza and envoy-coraza split rule text into
// directives: blank and comment lines are dropped, a line ending with a
// backtick opens a block that the next line starting with one closes, and a
// line ending with a backslash continues onto the next. It returns the first
// line of each directive and the text of a continuation left open at the end.
// Both engines refuse text that ends inside a block; open reports it, and the
// directive that opened the block is returned as well, as the checks read it.
func engineDirectives(rules string) (directives []string, dangling string, open bool) {
	var directive strings.Builder
	inBlock := false
	for line := range strings.SplitSeq(rules, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if !inBlock && line[len(line)-1] == '`' {
			inBlock = true
		} else if inBlock && line[0] == '`' {
			inBlock = false
		}
		if inBlock {
			directive.WriteString(line + "\n")
			continue
		}
		if strings.HasSuffix(line, `\`) {
			directive.WriteString(strings.TrimSuffix(line, `\`))
			continue
		}
		directive.WriteString(line)
		first, _, _ := strings.Cut(directive.String(), "\n")
		directives = append(directives, first)
		directive.Reset()
	}
	if inBlock {
		first, _, _ := strings.Cut(directive.String(), "\n")
		return append(directives, first), "", true
	}
	return directives, directive.String(), false
}

// TestScanDirectivesMatchesEngines guards against changes to how rule text is
// split into directives: it compares scanDirectives and joinActiveRuleLines
// with the engineDirectives model on random rule text built from the
// characters that change how lines are split, including text that ends inside
// a block or a continuation. TestScanDirectivesGoldenSplits ties the split to
// envoy-coraza.
func TestScanDirectivesMatchesEngines(t *testing.T) {
	alphabet := []string{"S", "x", " ", "\t", "\r", `\`, "`", "`", "\n", "\n", "\n", "#"}
	random := rand.New(rand.NewSource(1))
	for range 200_000 {
		var text strings.Builder
		for i := random.Intn(18); i > 0; i-- {
			text.WriteString(alphabet[random.Intn(len(alphabet))])
		}
		rules := text.String()
		want, wantDangling, open := engineDirectives(rules)
		got, dangling, err := scanDirectives(rules)
		require.Equal(t, want, got, "%q", rules)
		if open {
			require.ErrorIs(t, err, errBacktickClose, "%q", rules)
			continue
		}
		require.Equal(t, wantDangling, dangling, "%q", rules)
		joined := ""
		for _, line := range want {
			joined += line + "\n"
		}
		require.Equal(t, joined+wantDangling, joinActiveRuleLines(rules), "%q", rules)
	}
}

// TestScanDirectivesGoldenSplits checks scanDirectives against the directives
// envoy-coraza reads at the pinned coraza-rs commit (Parser::parse_string):
// the first line of each, and whether a backtick block is left open, which
// both engines refuse.
func TestScanDirectivesGoldenSplits(t *testing.T) {
	tests := []struct {
		rules string
		want  []string
		open  bool
	}{
		{rules: "SecAction \"id:1,\\\n\t phase:1\"\r\n# note\nSecMarker END", want: []string{`SecAction "id:1,phase:1"`, "SecMarker END"}},
		{rules: "SecMarker A\\\n\n  # comment\nB", want: []string{"SecMarker AB"}},
		{rules: "SecMarker A\\\\\nSecMarker B", want: []string{`SecMarker A\SecMarker B`}},
		{rules: "\t#SecMarker A\n \t\r\nSecMarker B\t\r", want: []string{"SecMarker B"}},
		{rules: "SecDataset words `\nfoo\\\n# bar\n`\nSecMarker END", want: []string{"SecDataset words `", "SecMarker END"}},
		{rules: "SecDataset a `\nx\n` \\\nSecDataset b `\ny\n`\nSecMarker END", want: []string{"SecDataset a `", "SecMarker END"}},
		{rules: "SecMarker A\\\n`\nx\n`", want: []string{"SecMarker A`"}},
		{rules: "x `\n`y\nSecMarker END", want: []string{"x `", "SecMarker END"}},
		{rules: "SecMarker Z\nSecMarker A \\", want: []string{"SecMarker Z"}},
		{rules: "SecMarker A\nSecDataset w `\nfoo", want: []string{"SecMarker A"}, open: true},
	}
	for _, tt := range tests {
		got, _, err := scanDirectives(tt.rules)
		if tt.open {
			require.ErrorIs(t, err, errBacktickClose, "%q", tt.rules)
			got = got[:len(got)-1] // the directive that opened the block
		}
		require.Equal(t, tt.want, got, "%q", tt.rules)
	}
}
