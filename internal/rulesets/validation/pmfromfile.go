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
	"fmt"
	"path"
	"regexp"
	"strings"
)

// pmFromFilePattern matches a quoted @pmFromFile or @pmf operator and its whole
// argument. The alias must be covered too, or @pmf can read any operator-local
// file. Matching the enclosing quotes keeps every token visible: Coraza treats
// the entire argument as one file name, so "@pmf a.data b.data" fails at build
// time even when a.data exists. The operator must start the quoted argument, so
// the same text inside another operator ("@rx @pmf foo") or an action message
// ("msg:'use @pmf foo'") is not scanned.
var pmFromFilePattern = regexp.MustCompile(`(?i)"(!?@(?:pmFromFile|pmf))[ \t]+([^"\n]*)"`)

// joinActiveRuleLines removes SecLang comment lines and joins trailing-backslash
// continuations exactly like Coraza's parser, so a continued @pmf argument
// cannot hide from the scan.
func joinActiveRuleLines(rules string) string {
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

// isEmbeddedDataPath reports whether a @pmFromFile target is served by the
// plugin's embedded rules tree (the CRS data files that sit next to the
// included CRS rules) rather than by RuleData. The operator cannot verify the
// embedded file list, so a typo there is a delivery risk, not a validation
// error.
func isEmbeddedDataPath(name string) bool {
	return strings.HasPrefix(name, "crs/")
}

// ValidatePMFromFileData ensures every @pmFromFile reference has a matching
// key in dataFiles (RuleSet-merged RuleData). Coraza may otherwise resolve
// files from the operator process working directory, which is unsafe and
// makes tests environment-dependent.
func ValidatePMFromFileData(aggregatedRules string, dataFiles map[string][]byte) error {
	activeRules := joinActiveRuleLines(aggregatedRules)
	for _, match := range pmFromFilePattern.FindAllStringSubmatch(activeRules, -1) {
		if len(match) < 3 {
			continue
		}
		operator := match[1]
		argument := strings.TrimSpace(match[2])
		if argument == "" {
			continue
		}
		if len(strings.Fields(argument)) > 1 {
			return fmt.Errorf("%s %s: expected a single data file name, but Coraza treats the whole argument as one file name", operator, argument)
		}
		// Coraza path-cleans the argument, so ./a.data resolves to a.data.
		basename := path.Clean(strings.Trim(argument, `"'`))
		if basename == "" || basename == "." {
			continue
		}
		if isEmbeddedDataPath(basename) {
			continue
		}
		if dataFiles == nil {
			return fmt.Errorf("%s %s requires spec.data RuleData but none are referenced", operator, basename)
		}
		if _, ok := dataFiles[basename]; !ok {
			return fmt.Errorf("%s %s: file not provided by referenced RuleData", operator, basename)
		}
	}
	return nil
}
