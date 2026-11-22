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
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// RulePolicy is the cluster rule policy: it narrows the rule text the operator
// accepts below what envoy-coraza supports and never allows more. The zero
// value denies nothing.
type RulePolicy struct {
	// DeniedOperators are operator names in their registered spelling.
	DeniedOperators []string
	// DeniedDirectives are directive names, matched case-insensitively.
	DeniedDirectives []string
	// MaxRequestBodyLimit is the highest SecRequestBodyLimit accepted. Zero
	// leaves only the envoy-coraza ceiling.
	MaxRequestBodyLimit int64
	// DenyResponseBodyAccess rejects SecResponseBodyAccess On.
	DenyResponseBodyAccess bool
}

// Response body access settings accepted by ParseRulePolicy.
const (
	ResponseBodyAccessAllowed = "Allowed"
	ResponseBodyAccessDenied  = "Denied"
)

// supportedOperators are the operators envoy-coraza supports, in the spelling
// Go Coraza registers them under.
var supportedOperators = []string{
	"beginsWith", "contains", "detectSQLi", "detectXSS", "endsWith", "eq", "ge",
	"geoLookup", "gt", "ipMatch", "ipMatchF", "ipMatchFromDataset",
	"ipMatchFromFile", "le", "lt", "noMatch", "pm", "pmf", "pmFromDataset",
	"pmFromFile", "restpath", "rx", "streq", "strmatch", "unconditionalMatch",
	"validateByteRange", "validateNid", "validateUrlEncoding",
	"validateUtf8Encoding", "within",
}

// operatorAliases maps each operator alias to the operator it stands for in
// both engines, so denying either name denies both.
var operatorAliases = map[string]string{
	"pmf":       "pmFromFile",
	"ipMatchF":  "ipMatchFromFile",
	"geoLookup": "unconditionalMatch",
}

// supportedDirectives are the directives envoy-coraza handles, in their
// documented spelling. Directives it accepts and ignores are not listed.
var supportedDirectives = []string{
	"Include", "SecAction", "SecArgumentsLimit", "SecAuditEngine", "SecAuditLog",
	"SecAuditLogDirMode", "SecAuditLogFileMode", "SecAuditLogFormat",
	"SecAuditLogParts", "SecAuditLogRelevantStatus", "SecAuditLogStorageDir",
	"SecAuditLogType", "SecCollectionTimeout", "SecComponentSignature",
	"SecDataDir", "SecDataset", "SecDebugLog", "SecDebugLogLevel",
	"SecDefaultAction", "SecIgnoreRuleCompilationErrors", "SecMarker",
	"SecRemoteRules", "SecRemoteRulesFailAction", "SecRequestBodyAccess",
	"SecRequestBodyInMemoryLimit", "SecRequestBodyJsonDepthLimit",
	"SecRequestBodyLimit", "SecRequestBodyLimitAction",
	"SecRequestBodyNoFilesLimit", "SecResponseBodyAccess",
	"SecResponseBodyJsonDepthLimit", "SecResponseBodyLimit",
	"SecResponseBodyLimitAction", "SecResponseBodyMimeType",
	"SecResponseBodyMimeTypesClear", "SecRule", "SecRuleEngine",
	"SecRuleRemoveById", "SecRuleRemoveByMsg", "SecRuleRemoveByTag",
	"SecRuleUpdateActionById", "SecRuleUpdateTargetById",
	"SecRuleUpdateTargetByTag", "SecRxPreFilter", "SecSensorId",
	"SecServerSignature", "SecUploadDir", "SecUploadFileLimit",
	"SecUploadFileMode", "SecUploadKeepFiles", "SecWebAppId",
}

// ctlDirectives maps each ctl action that both engines implement to the
// directives with the same effect, so denying a directive denies the action.
// envoy-coraza has no SecRuleUpdateTargetByMsg, so ruleRemoveTargetByMsg is
// tied to the other target updates.
var ctlDirectives = map[string][]string{
	"auditEngine":           {"SecAuditEngine"},
	"auditLogParts":         {"SecAuditLogParts"},
	"debugLogLevel":         {"SecDebugLogLevel"},
	"requestBodyAccess":     {"SecRequestBodyAccess"},
	"requestBodyLimit":      {"SecRequestBodyLimit"},
	"responseBodyAccess":    {"SecResponseBodyAccess"},
	"responseBodyLimit":     {"SecResponseBodyLimit"},
	"ruleEngine":            {"SecRuleEngine"},
	"ruleRemoveById":        {"SecRuleRemoveById"},
	"ruleRemoveByMsg":       {"SecRuleRemoveByMsg"},
	"ruleRemoveByTag":       {"SecRuleRemoveByTag"},
	"ruleRemoveTargetById":  {"SecRuleUpdateTargetById"},
	"ruleRemoveTargetByTag": {"SecRuleUpdateTargetByTag"},
	"ruleRemoveTargetByMsg": {"SecRuleUpdateTargetById", "SecRuleUpdateTargetByTag"},
}

var (
	// operatorNames, directiveNames and ctlNames map each lower-cased name to
	// its spelling above; an operator alias maps to the operator it stands for.
	operatorNames  = lowerNames(supportedOperators, operatorAliases)
	directiveNames = lowerNames(supportedDirectives, nil)
	ctlNames       = lowerNames(slices.Collect(maps.Keys(ctlDirectives)), nil)
)

func lowerNames(names []string, aliases map[string]string) map[string]string {
	lower := make(map[string]string, len(names))
	for _, name := range names {
		lower[strings.ToLower(name)] = name
	}
	for alias, name := range aliases {
		lower[strings.ToLower(alias)] = name
	}
	return lower
}

// responseBodyAccessPattern matches SecResponseBodyAccess and its value. Coraza
// enables response body inspection only for "On".
var responseBodyAccessPattern = regexp.MustCompile(`(?i)^secresponsebodyaccess[ \t]+(\S+)`)

// ParseRulePolicy builds the cluster rule policy from the manager flags:
// comma-separated operator and directive names, the request body ceiling, and
// the response body access setting.
func ParseRulePolicy(deniedOperators, deniedDirectives string, maxRequestBodyLimit int64, responseBodyAccess string) (RulePolicy, error) {
	var p RulePolicy
	operators, err := splitNames("rule-denied-operators", deniedOperators)
	if err != nil {
		return RulePolicy{}, err
	}
	for _, name := range operators {
		canonical, ok := operatorNames[strings.ToLower(name)]
		if !ok {
			return RulePolicy{}, fmt.Errorf("rule-denied-operators: %q is not an operator envoy-coraza supports", name)
		}
		p.DeniedOperators = append(p.DeniedOperators, canonical)
	}
	directives, err := splitNames("rule-denied-directives", deniedDirectives)
	if err != nil {
		return RulePolicy{}, err
	}
	for _, name := range directives {
		canonical, ok := directiveNames[strings.ToLower(name)]
		if !ok {
			return RulePolicy{}, fmt.Errorf("rule-denied-directives: %q is not a directive envoy-coraza supports", name)
		}
		p.DeniedDirectives = append(p.DeniedDirectives, canonical)
	}
	if maxRequestBodyLimit <= 0 || maxRequestBodyLimit > MaxRequestBodyLimit {
		return RulePolicy{}, fmt.Errorf("rule-max-request-body-limit %d must be within (0, %d] bytes", maxRequestBodyLimit, MaxRequestBodyLimit)
	}
	// At the ceiling envoy-coraza already clamps every limit, so the policy sets none.
	if maxRequestBodyLimit < MaxRequestBodyLimit {
		p.MaxRequestBodyLimit = maxRequestBodyLimit
	}
	switch responseBodyAccess {
	case ResponseBodyAccessAllowed:
	case ResponseBodyAccessDenied:
		p.DenyResponseBodyAccess = true
	default:
		return RulePolicy{}, fmt.Errorf("rule-response-body-access %q must be %s or %s", responseBodyAccess, ResponseBodyAccessAllowed, ResponseBodyAccessDenied)
	}
	return p, nil
}

// splitNames splits a comma-separated flag value; an empty value has no names.
func splitNames(flag, value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	names := strings.Split(value, ",")
	for i, name := range names {
		names[i] = strings.TrimSpace(name)
		if names[i] == "" {
			return nil, fmt.Errorf("%s: empty entry in %q", flag, value)
		}
	}
	return names, nil
}

// CheckRulePolicy rejects rule text the cluster rule policy denies, in the rules
// and in every RuleData file they Include. It runs after ValidateIncludes and
// CheckGatewayParity, so the rule text already meets what envoy-coraza supports.
// A ctl action is judged like the directive with the same effect.
func CheckRulePolicy(rules string, dataFiles map[string][]byte, p RulePolicy) error {
	// envoy-coraza caps a body that ctl:requestBodyAccess=On enables at the
	// configured SecRequestBodyLimit, or at its 16 MiB ceiling when none is set.
	limitCeiling := p.MaxRequestBodyLimit > 0
	hasLimit := false
	if limitCeiling {
		_ = forEachRuleText(rules, dataFiles, func(_, text string) error {
			hasLimit = hasLimit || slices.ContainsFunc(directiveLines(text), requestBodyLimitPattern.MatchString)
			return nil
		})
	}
	return checkEachText(rules, dataFiles, func(text string) error {
		return checkPolicyText(text, p, limitCeiling && !hasLimit)
	})
}

func checkPolicyText(rules string, p RulePolicy, needsLimit bool) error {
	for _, line := range directiveLines(rules) {
		directive, _, _ := strings.Cut(line, " ")
		if denied, ok := deniedDirective(p, directive); ok {
			return fmt.Errorf("%s is not allowed by the cluster rule policy", denied)
		}
		if len(p.DeniedOperators) > 0 {
			if name, ok := ruleOperatorName(line); ok && slices.Contains(p.DeniedOperators, name) {
				return fmt.Errorf("@%s is not allowed by the cluster rule policy", name)
			}
		}
		if p.MaxRequestBodyLimit > 0 {
			if match := requestBodyLimitPattern.FindStringSubmatch(line); match != nil {
				limit, err := strconv.ParseInt(strings.Trim(match[1], `"'`), 10, 64)
				if err == nil && limit > p.MaxRequestBodyLimit {
					return fmt.Errorf("SecRequestBodyLimit %d exceeds the cluster rule policy maximum of %d bytes", limit, p.MaxRequestBodyLimit)
				}
			}
		}
		if p.DenyResponseBodyAccess {
			if match := responseBodyAccessPattern.FindStringSubmatch(line); match != nil && strings.EqualFold(strings.Trim(match[1], `"'`), "on") {
				return errors.New("SecResponseBodyAccess On is not allowed by the cluster rule policy")
			}
		}
		for _, ctl := range ctlActions(line) {
			if err := checkCtl(ctl[0], ctl[1], p, needsLimit); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCtl judges one ctl action like its directive.
func checkCtl(name, value string, p RulePolicy, needsLimit bool) error {
	ctl, ok := ctlNames[strings.ToLower(name)]
	if !ok {
		return nil
	}
	for _, directive := range ctlDirectives[ctl] {
		if denied, ok := deniedDirective(p, directive); ok {
			return fmt.Errorf("ctl:%s is not allowed by the cluster rule policy: it acts like %s", ctl, denied)
		}
	}
	on := strings.EqualFold(value, "on")
	switch ctl {
	case "responseBodyAccess":
		if on && p.DenyResponseBodyAccess {
			return errors.New("ctl:responseBodyAccess=On is not allowed by the cluster rule policy")
		}
	case "requestBodyAccess":
		if on && needsLimit {
			return fmt.Errorf("ctl:requestBodyAccess=On needs a SecRequestBodyLimit at or below the cluster rule policy maximum of %d bytes: without one, envoy-coraza inspects up to %d bytes", p.MaxRequestBodyLimit, MaxRequestBodyLimit)
		}
	case "requestBodyLimit":
		if limit, err := strconv.ParseInt(value, 10, 64); err == nil && p.MaxRequestBodyLimit > 0 && limit > p.MaxRequestBodyLimit {
			return fmt.Errorf("ctl:requestBodyLimit=%d exceeds the cluster rule policy maximum of %d bytes", limit, p.MaxRequestBodyLimit)
		}
	}
	return nil
}

func deniedDirective(p RulePolicy, directive string) (string, bool) {
	for _, denied := range p.DeniedDirectives {
		if strings.EqualFold(directive, denied) {
			return denied, true
		}
	}
	return "", false
}

// ctlActions returns the ctl actions of a directive line that carries actions
// in either engine (SecRule, SecAction, SecDefaultAction and
// SecRuleUpdateActionById) as name and value pairs. Each action list is split
// as Coraza splits it: on commas outside single quotes, each action at its
// first colon, and a ctl value at "=" and ";". Quotes left at the edges of an
// action, its name or its value are ignored.
func ctlActions(line string) [][2]string {
	var ctls [][2]string
	for _, actions := range actionLists(line) {
		for _, action := range splitActionList(trimQuotes(strings.TrimSpace(actions))) {
			key, value, _ := strings.Cut(action, ":")
			if !strings.EqualFold(strings.TrimLeft(strings.TrimSpace(key), `"`), "ctl") {
				continue
			}
			name, setting, _ := strings.Cut(trimQuotes(strings.TrimSpace(value)), "=")
			setting, _, _ = strings.Cut(setting, ";")
			ctls = append(ctls, [2]string{strings.TrimSpace(name), strings.Trim(strings.TrimSpace(setting), `"`)})
		}
	}
	return ctls
}

// actionLists returns the action lists of a directive line. For
// SecRuleUpdateActionById the engines differ, so both readings are returned,
// each with every leading and trailing double quote stripped as that engine
// strips it: Coraza takes the last whitespace-separated field, envoy-coraza
// the last double-quoted string, or the last field when the arguments do not
// end with a quote.
func actionLists(line string) []string {
	actions, ids, ok := actionArgument(line)
	if !ok {
		return nil
	}
	if ids == nil {
		return []string{actions}
	}
	_, _, envoyOpts := directiveArguments(line)
	_, last, _ := envoySplitLast(envoyOpts)
	return []string{strings.Trim(actions, `"`), last}
}

// splitActionList splits an action list on commas outside single quotes; a
// backslash escapes the character after it.
func splitActionList(actions string) []string {
	var parts []string
	inQuotes := false
	start := 0
	for i := 0; i < len(actions); i++ {
		switch {
		case i > 0 && actions[i-1] == '\\':
		case actions[i] == '\'':
			inQuotes = !inQuotes
		case actions[i] == ',' && !inQuotes:
			parts = append(parts, actions[start:i])
			start = i + 1
		}
	}
	return append(parts, actions[start:])
}

// trimQuotes removes one pair of matching single or double quotes.
func trimQuotes(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// ruleOperatorName returns the registered name of a SecRule line's operator as
// envoy-coraza resolves it: leading whitespace and a "!" are dropped, the name
// after "@" ends at the first space and is matched case-insensitively, and an
// argument without "@" is the default @rx.
func ruleOperatorName(line string) (string, bool) {
	op, _, ok := secRuleOperator(line)
	if !ok {
		return "", false
	}
	op = strings.TrimPrefix(strings.TrimLeftFunc(op, unicode.IsSpace), "!")
	name, named := strings.CutPrefix(op, "@")
	if !named {
		return "rx", true
	}
	name, _, _ = strings.Cut(name, " ")
	canonical, ok := operatorNames[strings.ToLower(strings.TrimRightFunc(name, unicode.IsSpace))]
	return canonical, ok
}
