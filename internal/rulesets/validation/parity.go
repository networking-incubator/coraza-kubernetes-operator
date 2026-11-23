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
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxLineBytes is the length at which Coraza's line scanner (bufio.Scanner with
// its default 64 KiB token size) stops reading rule text without an error.
const maxLineBytes = 64 << 10

var (
	// errLeadingSpaceOperator rejects an operator whose quoted argument starts
	// with whitespace before "@" or "!". Coraza reads it as an @rx pattern,
	// while envoy-coraza trims the operator first and reads the operator or
	// negation, so the gateway would run a different rule than the one
	// validated.
	errLeadingSpaceOperator = errors.New(`SecRule operator must not start with whitespace before "@" or "!": envoy-coraza trims the operator, so the whitespace changes its meaning`)

	// errIgnoreCompilationErrors rejects SecIgnoreRuleCompilationErrors On:
	// Coraza would skip a rule it cannot compile instead of reporting it, and
	// envoy-coraza refuses a configuration in which a directive was skipped.
	errIgnoreCompilationErrors = errors.New("SecIgnoreRuleCompilationErrors On is not supported: Coraza would skip a rule it cannot compile instead of reporting it, and envoy-coraza refuses a configuration in which a directive was skipped")

	// errBacktickBlock and errBacktickClose reject backtick blocks other than a
	// SecDataset item list closed by a line holding just a backtick.
	errBacktickBlock = errors.New("a backtick block is only supported to list SecDataset items: the lines of a block belong to the directive that opens it")
	errBacktickClose = errors.New("a SecDataset block must close with a line holding just a backtick")

	// errUpdateActionIDs rejects SecRuleUpdateActionById with another ID or
	// range after a single rule ID: Coraza updates only up to the first single
	// ID, while envoy-coraza updates every ID and range listed.
	errUpdateActionIDs = errors.New("SecRuleUpdateActionById must not list another ID or range after a single rule ID: Coraza stops at the first single ID, while envoy-coraza updates every ID and range listed")
)

// CheckDirectiveSyntax rejects, in the rules and in every RuleData file they
// Include, a directive name with a non-ASCII letter, which Coraza and
// envoy-coraza lowercase to different directives, and a backtick block other
// than a SecDataset item list closed by a line holding just a backtick, whose
// lines the checks in this package could split differently from the engines.
// It runs before every Coraza compile of tenant rule text.
func CheckDirectiveSyntax(rules string, dataFiles map[string][]byte) error {
	return checkEachText(rules, dataFiles, checkDirectiveSyntax)
}

// CheckGatewayParity rejects rule text that Coraza, which validates it, and
// envoy-coraza, which runs it, would read differently, in the rules and in every
// RuleData file they Include. It runs before Coraza compiles new RuleSet
// content, so a rule Coraza would skip or read another way never passes
// validation.
func CheckGatewayParity(rules string, dataFiles map[string][]byte) error {
	return checkEachText(rules, dataFiles, checkParityText)
}

// checkEachText runs check on the rules and on every RuleData file they
// Include, naming the included file in its error.
func checkEachText(rules string, dataFiles map[string][]byte, check func(string) error) error {
	return forEachRuleText(rules, dataFiles, func(name, text string) error {
		err := check(text)
		if err != nil && name != "" {
			return fmt.Errorf("included RuleData file %s: %w", name, err)
		}
		return err
	})
}

func checkDirectiveSyntax(rules string) error {
	lines, _, err := scanDirectives(rules)
	if err != nil {
		return err
	}
	return checkNames(lines)
}

func checkNames(lines []string) error {
	for _, line := range lines {
		if directive, _, _ := strings.Cut(line, " "); !isASCII(directive) {
			return fmt.Errorf("directive %s is not supported: directive names are ASCII, and Coraza and envoy-coraza lowercase other letters differently", quoteBounded(directive))
		}
	}
	return nil
}

func checkParityText(rules string) error {
	number := 0
	for line := range strings.SplitSeq(rules, "\n") {
		number++
		if len(line) >= maxLineBytes {
			return fmt.Errorf("line %d is %d bytes: lines of %d bytes or more are not supported, because Coraza stops reading rule text there; split it with a trailing backslash", number, len(line), maxLineBytes)
		}
	}
	lines, _, err := scanDirectives(rules)
	if err != nil {
		return err
	}
	if err := checkNames(lines); err != nil {
		return err
	}
	for _, line := range lines {
		directive, opts, envoyOpts := directiveArguments(line)
		if opts != envoyOpts {
			return fmt.Errorf("directive %s: arguments in double quotes must not be empty or start or end with another double quote, because Coraza and envoy-coraza remove those quotes differently", quoteBounded(directive))
		}
		if strings.EqualFold(directive, "SecIgnoreRuleCompilationErrors") && strings.EqualFold(opts, "on") {
			return errIgnoreCompilationErrors
		}
		if err := checkOperatorSpacing(line); err != nil {
			return err
		}
		if err := checkActionList(line); err != nil {
			return err
		}
		if err := checkUpdateArguments(directive, opts, envoyOpts); err != nil {
			return err
		}
	}
	return nil
}

// checkActionList rejects an action list with unbalanced double quotes on a
// directive that carries actions, a SecRuleUpdateActionById action list with a
// double quote, escaped or not, inside its enclosing pair (the engines take
// different parts of it), and a SecRuleUpdateActionById ID list the engines
// apply differently: a single rule ID, or a range of one ID, followed by
// another ID or range.
func checkActionList(line string) error {
	actions, ids, ok := actionArgument(line)
	if !ok {
		return nil
	}
	directive, _, _ := strings.Cut(line, " ")
	quoted := strings.HasPrefix(actions, `"`)
	// Both engines close a SecRule action list at its last character. In the
	// other action lists, a quote escaped with a backslash is part of an action
	// value, except in SecRuleUpdateActionById: envoy-coraza splits it at the
	// last double quote whether or not it is escaped, so its action list must
	// not end with one unless the list is enclosed in an unescaped pair.
	closed := endsWithQuote(actions)
	if strings.EqualFold(directive, "SecRule") {
		closed = strings.HasSuffix(actions, `"`)
	}
	if quoted != closed || actions == `"` ||
		(ids != nil && (quoted != strings.HasSuffix(actions, `"`) || quoted && strings.Contains(actions[1:len(actions)-1], `"`))) {
		return fmt.Errorf("%s: an action list must be enclosed in one pair of double quotes, or in none", directive)
	}
	for i := 0; i+1 < len(ids); i++ {
		low, high, isRange := strings.Cut(ids[i], "-")
		start, startErr := strconv.Atoi(low)
		end, endErr := strconv.Atoi(high)
		if !isRange || (startErr == nil && endErr == nil && start == end) {
			return errUpdateActionIDs
		}
	}
	return nil
}

// checkUpdateArguments rejects SecRuleUpdateActionById,
// SecRuleUpdateTargetById and SecRuleUpdateTargetByTag arguments that Coraza
// and envoy-coraza split differently. Coraza takes whitespace-separated
// fields: the last one, with its double quotes trimmed, is the action or
// variable list, and the fields before it are the rule IDs or the tag, which
// it also trims. envoy-coraza reads them with envoySplitLast.
func checkUpdateArguments(directive, opts, envoyOpts string) error {
	byTag := strings.EqualFold(directive, "SecRuleUpdateTargetByTag")
	if !byTag && !strings.EqualFold(directive, "SecRuleUpdateActionById") && !strings.EqualFold(directive, "SecRuleUpdateTargetById") {
		return nil
	}
	fields := strings.Fields(opts)
	if len(fields) < 2 || byTag && len(fields) != 2 {
		return nil // Coraza refuses these arguments.
	}
	rest, last, ok := envoySplitLast(envoyOpts)
	before := strings.Fields(rest)
	if byTag {
		before, fields[0] = []string{rest}, strings.Trim(fields[0], `"`)
	}
	if !ok || last != strings.Trim(fields[len(fields)-1], `"`) || !slices.Equal(before, fields[:len(fields)-1]) {
		return fmt.Errorf("%s: the last argument must be one field, enclosed in one pair of double quotes or in none, with no other double quote: Coraza and envoy-coraza split these arguments differently", directive)
	}
	return nil
}

// envoySplitLast splits a directive's arguments, as envoy-coraza reads them
// (see directiveArguments), at the last one: the text after the last double
// quote when the arguments end with a quote, otherwise the last
// whitespace-separated field. Double quotes around either part are removed;
// ok is false when either part is empty, which envoy-coraza refuses.
func envoySplitLast(opts string) (rest, last string, ok bool) {
	opts = strings.TrimSpace(opts)
	i, size := strings.LastIndexFunc(opts, unicode.IsSpace), 1
	if quoted, found := strings.CutSuffix(opts, `"`); found {
		opts, i = quoted, strings.LastIndex(quoted, `"`)
	} else if i >= 0 {
		_, size = utf8.DecodeRuneInString(opts[i:])
	}
	if i < 0 {
		return "", "", false
	}
	rest = strings.Trim(strings.TrimSpace(opts[:i]), `"`)
	last = strings.Trim(opts[i+size:], `"`)
	return rest, last, rest != "" && last != ""
}

// endsWithQuote reports whether s ends with a double quote that is not escaped
// with a backslash.
func endsWithQuote(s string) bool {
	if !strings.HasSuffix(s, `"`) {
		return false
	}
	body := s[:len(s)-1]
	return (len(body)-len(strings.TrimRight(body, `\`)))%2 == 0
}

// unescapedQuote returns the index of the first double quote in s that is not
// escaped with a backslash, or -1.
func unescapedQuote(s string) int {
	escapes := 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			escapes++
		case s[i] == '"' && escapes%2 == 0:
			return i
		default:
			escapes = 0
		}
	}
	return -1
}

// actionArgument returns the action list argument of a directive line that
// carries actions (SecRule, SecAction, SecDefaultAction and
// SecRuleUpdateActionById) as Coraza takes it, quotes included, and for
// SecRuleUpdateActionById the rule IDs and ranges before it.
func actionArgument(line string) (actions string, ids []string, ok bool) {
	directive, opts, _ := directiveArguments(line)
	opts = strings.TrimSpace(opts)
	switch strings.ToLower(directive) {
	case "secrule":
		_, actions, ok = secRuleOperator(line)
		return actions, nil, ok
	case "secaction", "secdefaultaction":
		return opts, nil, true
	case "secruleupdateactionbyid":
		fields := strings.Fields(opts)
		if len(fields) < 2 {
			return "", nil, false
		}
		return fields[len(fields)-1], fields[:len(fields)-1], true
	}
	return "", nil, false
}

// directiveArguments splits a directive line into its name and its arguments
// as Coraza and envoy-coraza read them. When the arguments are enclosed in
// double quotes, Coraza removes every leading and trailing one (and only from
// three bytes on), envoy-coraza one pair.
func directiveArguments(line string) (directive, coraza, envoy string) {
	directive, opts, _ := strings.Cut(line, " ")
	envoy = opts
	if len(opts) >= 2 && opts[0] == '"' && opts[len(opts)-1] == '"' {
		envoy = opts[1 : len(opts)-1]
	}
	return directive, includeTarget(opts), envoy
}

// quoteBounded quotes rule text for a message, cut to its first 64 bytes on a
// rune boundary.
func quoteBounded(s string) string {
	const limit = 64
	if len(s) <= limit {
		return strconv.Quote(s)
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strconv.Quote(s[:cut]) + "..."
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// directiveLines returns the directives of rule text; see scanDirectives.
func directiveLines(rules string) []string {
	lines, _, _ := scanDirectives(rules)
	return lines
}

// scanDirectives returns the directives of rule text as Coraza's and
// envoy-coraza's parsers assemble them, line by line in the same way: blank
// and comment lines are dropped, a line ending with a backtick opens a block
// that the next line starting with one closes, and a line ending with a
// backslash continues the directive onto the next. Each directive is returned
// as its first line, so the lines of a block, which the engines pass to the
// directive that opened it, are read as data. The error reports any block
// other than a SecDataset item list closed by a line holding just a backtick.
// dangling is a continuation left open at the end of the text, which neither
// engine evaluates.
func scanDirectives(rules string) (lines []string, dangling string, blockErr error) {
	dangling, blockErr = walkDirectives(rules, func(line string) bool {
		lines = append(lines, line)
		return true
	})
	return lines, dangling, blockErr
}

// walkDirectives calls yield with each directive of rule text, as
// scanDirectives returns them, until yield returns false.
func walkDirectives(rules string, yield func(line string) bool) (dangling string, blockErr error) {
	var directive strings.Builder
	inBlock := false
	for line := range strings.SplitSeq(rules, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		switch {
		case !inBlock && line[len(line)-1] == '`':
			inBlock = true
			if name, _, _ := strings.Cut(directive.String()+line, " "); !strings.EqualFold(name, "SecDataset") && blockErr == nil {
				blockErr = errBacktickBlock
			}
		case inBlock && line[0] == '`':
			inBlock = false
			if line != "`" && blockErr == nil {
				blockErr = errBacktickClose
			}
		}
		switch {
		case inBlock:
			directive.WriteString(line)
			directive.WriteByte('\n')
		case strings.HasSuffix(line, `\`):
			directive.WriteString(strings.TrimSuffix(line, `\`))
		case directive.Len() == 0:
			if !yield(line) {
				return "", blockErr
			}
		default:
			directive.WriteString(line)
			first, _, _ := strings.Cut(directive.String(), "\n")
			directive.Reset()
			if !yield(first) {
				return "", blockErr
			}
		}
	}
	if inBlock {
		// Both engines refuse a block left open; its directive is still read.
		first, _, _ := strings.Cut(directive.String(), "\n")
		yield(first)
		return "", errBacktickClose
	}
	return directive.String(), blockErr
}

// checkOperatorSpacing returns errLeadingSpaceOperator for a SecRule line whose
// operator starts with whitespace followed by "@" or "!".
func checkOperatorSpacing(line string) error {
	op, _, ok := secRuleOperator(line)
	if !ok {
		return nil
	}
	trimmed := strings.TrimLeftFunc(op, unicode.IsSpace)
	if trimmed != op && (strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "!")) {
		return errLeadingSpaceOperator
	}
	return nil
}

// secRuleOperator returns the operator argument of a SecRule directive line and
// the actions after it, split as Coraza's parser does (evaluateLine and
// parseActionOperator): the directive ends at the first space, the variables
// at the next one, and the operator is the double-quoted string after them, in
// which a backslash escapes a quote.
func secRuleOperator(line string) (operator, actions string, ok bool) {
	directive, opts, _ := directiveArguments(line)
	if !strings.EqualFold(directive, "secrule") {
		return "", "", false
	}
	_, rest, found := strings.Cut(strings.Trim(opts, " "), " ")
	if !found {
		return "", "", false
	}
	rest = strings.TrimLeft(rest, " ")
	if !strings.HasPrefix(rest, `"`) {
		return "", "", false
	}
	end := unescapedQuote(rest[1:])
	if end < 0 {
		return "", "", false
	}
	return rest[1 : end+1], strings.TrimSpace(rest[end+2:]), true
}
