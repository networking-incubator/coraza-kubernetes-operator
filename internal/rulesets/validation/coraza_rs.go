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
	"path"
	"strings"
)

// ValidateCorazaRSIncludes rejects Include forms the envoy-coraza plugin cannot
// resolve. The previous Go plugin embedded its own alias filesystem
// (@owasp_crs/..., @recommended-conf, ...) and expanded globs inside it;
// coraza-rs has no such tree, so those targets fail at configuration time while
// the Engine reports Ready. RuleData file names are unaffected.
//
// An included RuleData file is parsed as rule text, so its Includes are checked
// too, following the same chain the plugin compiles.
func ValidateCorazaRSIncludes(rules string, dataFiles map[string][]byte) error {
	seen := make(map[string]struct{}, len(dataFiles))
	queue := []struct{ name, text string }{{text: rules}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if err := validateCorazaRSRuleText(current.text); err != nil {
			return err
		}
		for _, include := range ruleDataIncludeNames(current.text) {
			name := path.Join(path.Dir(current.name), include)
			if _, ok := seen[name]; ok {
				continue
			}
			data, ok := dataFiles[name]
			if !ok {
				continue
			}
			seen[name] = struct{}{}
			queue = append(queue, struct{ name, text string }{name: name, text: string(data)})
		}
	}
	return nil
}

func validateCorazaRSRuleText(rules string) error {
	for line := range strings.SplitSeq(joinActiveRuleLines(rules), "\n") {
		match := includePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		target := strings.TrimSpace(includeTarget(match[1]))
		if strings.HasPrefix(target, "@") {
			return errors.New(`include alias is not supported by the envoy-coraza plugin: use crs/crs-setup.conf and crs/rules/*.conf, a RuleData file name, or kubectl coraza generate coreruleset`)
		}
		if isGlob(target) && target != "crs/rules/*.conf" {
			return errors.New(`include with a glob is only supported as crs/rules/*.conf: use the embedded CRS or list RuleData files explicitly`)
		}
	}
	return nil
}
