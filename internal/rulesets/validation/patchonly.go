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
	"regexp"
	"strings"
)

var (
	secRuleDefLine     = regexp.MustCompile(`^\s*Sec(?:Rule|Action|Marker)\b`)
	patchDirectiveLine = regexp.MustCompile(`^\s*SecRule(?:Update|Remove)`)
	secDirectiveLine   = regexp.MustCompile(`^\s*Sec`)
)

// IsPatchOnlyFragment reports whether rules contain only configure-time patch
// directives (SecRuleUpdate*, SecRuleRemove*) and no SecRule/SecAction/SecMarker
// definitions. Such fragments cannot be validated in isolation because they
// reference rule IDs from other sources; RuleSet aggregate validation is authoritative.
func IsPatchOnlyFragment(rules string) bool {
	hasSecDirective := false
	for line := range strings.SplitSeq(rules, "\n") {
		stripped := strings.TrimSpace(line)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		if !secDirectiveLine.MatchString(stripped) {
			continue
		}
		hasSecDirective = true
		if secRuleDefLine.MatchString(stripped) {
			return false
		}
		if !patchDirectiveLine.MatchString(stripped) {
			return false
		}
	}
	return hasSecDirective
}
