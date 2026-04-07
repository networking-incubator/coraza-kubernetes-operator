package corerulesetgen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	secDirectiveLine = regexp.MustCompile(`^(SecRule|SecAction|SecMarker)\b`)
	ruleIDRe         = regexp.MustCompile(`id:(\d+)`)
	// chain in ModSecurity rule actions (e.g. ,chain, or ,chain")
	chainActionRe = regexp.MustCompile(`,\s*chain\s*(?:,|")`)
)

// trimLineEnd trims trailing CR/LF, spaces, and tabs. Continuation lines
// often end with '\' followed by spaces before the newline; those spaces
// must be ignored when detecting a backslash continuation.
func trimLineEnd(line string) string {
	return strings.TrimRight(line, " \t\r\n")
}

func splitIntoRules(content string) []string {
	lines := strings.Split(content, "\n")
	blocks := make([]string, 0, len(lines))
	var current []string
	inMultiline := false

	for _, line := range lines {
		stripped := trimLineEnd(line)
		if inMultiline {
			current = append(current, line)
			if !strings.HasSuffix(stripped, "\\") {
				inMultiline = false
				blocks = append(blocks, strings.Join(current, "\n"))
				current = nil
			}
			continue
		}
		if !strings.HasPrefix(stripped, "#") && secDirectiveLine.MatchString(stripped) {
			current = []string{line}
			if strings.HasSuffix(stripped, "\\") {
				inMultiline = true
			} else {
				blocks = append(blocks, strings.Join(current, "\n"))
				current = nil
			}
			continue
		}
		blocks = append(blocks, line)
	}
	if len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

func extractRuleID(ruleText string) string {
	m := ruleIDRe.FindStringSubmatch(ruleText)
	if m != nil {
		return m[1]
	}
	return "unknown"
}

func secRuleHasChainAction(block string) bool {
	return chainActionRe.MatchString(block)
}

// chainSecRuleGroups partitions SecRule block indices into ModSecurity chains.
// Each inner slice is one chain (multiple SecRules ending with a rule without
// chain) or a standalone SecRule (len 1). Comment and blank blocks are skipped
// for grouping but do not break a chain when they appear between chained SecRules.
func chainSecRuleGroups(blocks []string) [][]int {
	groups := make([][]int, 0, len(blocks))
	var cur []int
	inChain := false

	for i, block := range blocks {
		stripped := strings.TrimSpace(block)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		if !strings.HasPrefix(stripped, "SecRule") {
			if inChain {
				groups = append(groups, cur)
				cur = nil
				inChain = false
			}
			continue
		}
		if secRuleHasChainAction(block) {
			cur = append(cur, i)
			inChain = true
			continue
		}
		if inChain {
			cur = append(cur, i)
			groups = append(groups, cur)
			cur = nil
			inChain = false
			continue
		}
		groups = append(groups, []int{i})
	}
	if inChain && len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

func processFileContent(path string, ignoreIDs, autoIgnored map[string]struct{}, ignorePM bool) (string, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", path, err)
	}
	content := strings.TrimRight(strings.ToValidUTF8(string(raw), ""), "\n\r")
	if !strings.Contains(content, "SecRule") && !strings.Contains(content, "SecAction") {
		return "", nil, nil
	}

	var warns []string
	blocks := splitIntoRules(content)
	base := filepath.Base(path)

	wishDrop := make([]bool, len(blocks))
	for i, block := range blocks {
		stripped := strings.TrimSpace(block)
		if stripped == "" || strings.HasPrefix(stripped, "#") || !strings.HasPrefix(stripped, "Sec") {
			continue
		}
		if !strings.HasPrefix(stripped, "SecRule") {
			continue
		}
		if ignorePM && strings.Contains(block, "@pmFromFile") {
			wishDrop[i] = true
			continue
		}
		rid := extractRuleID(block)
		if _, drop := ignoreIDs[rid]; drop {
			wishDrop[i] = true
		}
	}

	groups := chainSecRuleGroups(blocks)
	drop := make([]bool, len(blocks))
	copy(drop, wishDrop)
	for _, g := range groups {
		any := false
		for _, i := range g {
			if wishDrop[i] {
				any = true
				break
			}
		}
		if len(g) <= 1 || !any {
			continue
		}
		for _, i := range g {
			drop[i] = true
		}
	}

	for _, g := range groups {
		any := false
		for _, i := range g {
			if wishDrop[i] {
				any = true
				break
			}
		}
		if !any {
			continue
		}
		if len(g) > 1 {
			ids := make([]string, 0, len(g))
			for _, i := range g {
				ids = append(ids, extractRuleID(blocks[i]))
			}
			warns = append(warns, fmt.Sprintf("  [warn] Ignored rules in %s:\n    - SecRule chain (IDs: %s): dropping entire chain because at least one rule matched --ignore-rules, default WASM-unsupported filtering, or @pmFromFile stripping\n", base, strings.Join(ids, ", ")))
			continue
		}
		i := g[0]
		if ignorePM && strings.Contains(blocks[i], "@pmFromFile") {
			warns = append(warns, fmt.Sprintf("  [warn] Ignored rules in %s:\n    - Rule ID: %s (@pmFromFile not supported)\n", base, extractRuleID(blocks[i])))
			continue
		}
		rid := extractRuleID(blocks[i])
		if _, d := ignoreIDs[rid]; d {
			reason := ignoreReason(rid, autoIgnored)
			warns = append(warns, fmt.Sprintf("  [warn] Ignored rules in %s:\n    - Rule ID: %s (%s)\n", base, rid, reason))
		}
	}

	filtered := make([]string, 0, len(blocks))
	for i, block := range blocks {
		if drop[i] {
			continue
		}
		filtered = append(filtered, block)
	}
	return strings.Join(filtered, "\n"), warns, nil
}

func ignoreReason(ruleID string, autoIgnored map[string]struct{}) string {
	if autoIgnored != nil {
		if _, w := autoIgnored[ruleID]; w {
			return "excluded by --ignore-unsupported-rules profile (operator); pass --ignore-unsupported-rules=none to emit this rule"
		}
		return "Rule ID in --ignore-rules"
	}
	return "Rule ID in ignore list"
}

func buildRuleSourceYAML(path string, opts Options) (name, yamlOut, skipReason string, warns []string, err error) {
	base := filepath.Base(path)
	rawName, err := generateRuleSourceName(base)
	if err != nil {
		return "", "", err.Error(), nil, err
	}
	name = opts.NamePrefix + rawName + opts.NameSuffix
	if name == "" {
		return "", "", "invalid empty RuleSource name", nil, fmt.Errorf("empty RuleSource name after prefix/suffix")
	}
	if err := validateRuleSourceObjectName(name); err != nil {
		return "", "", err.Error(), nil, err
	}

	processed, w, err := processFileContent(path, opts.IgnoreRuleIDs, opts.autoIgnoredIDs, opts.IgnorePMFromFile)
	if err != nil {
		return "", "", "", nil, err
	}
	warns = w
	if strings.TrimSpace(processed) == "" {
		return "", "", "No SecRule or SecAction directives found", warns, nil
	}

	indented := indentMultiline(processed, 4)
	payload := indented + "\n"
	if err := checkPayloadSize(payload, name, opts); err != nil {
		return "", "", "", warns, err
	}
	yamlOut = formatRuleSourceYAML(name, opts.Namespace, indented)
	return name, yamlOut, "", warns, nil
}
