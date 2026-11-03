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
// crs/ targets are rejected unless the module embeds CRS: without it they
// resolve to nothing and compile a zero-rule WAF while the Engine reports
// Ready. The bundled module's capability is the default; per-Engine rendering
// passes the selected module's capability explicitly.
//
// An included RuleData file is parsed as rule text, so its Includes are checked
// too, following the same chain the plugin compiles.
func ValidateCorazaRSIncludes(rules string, dataFiles map[string][]byte) error {
	// An empty bundled version means the manifest did not record one; treat it
	// as a known no-CRS module so the historical contract is unchanged.
	version := BundledCRSVersion
	if version == "" {
		version = "none"
	}
	return ValidateCorazaRSIncludesForModule(rules, dataFiles, version)
}

// ValidateCorazaRSIncludesForModule validates Include forms against the CRS
// capability of the selected module. crsVersion is the embedded OWASP CRS
// release: "none" is a known no-CRS module and an empty string is an unknown
// capability. Unknown is rejected conservatively: accepting a crs/ reference
// for a module that cannot resolve it serves a zero-rule WAF.
func ValidateCorazaRSIncludesForModule(rules string, dataFiles map[string][]byte, crsVersion string) error {
	seen := make(map[string]struct{}, len(dataFiles))
	queue := []struct{ name, text string }{{text: rules}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if err := validateCorazaRSRuleText(current.text, crsVersion); err != nil {
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

func validateCorazaRSRuleText(rules, crsVersion string) error {
	active := joinActiveRuleLines(rules)
	embedsCRS := crsVersion != "" && crsVersion != "none"
	for line := range strings.SplitSeq(active, "\n") {
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
		if !embedsCRS && isRuntimeResolvedPath(target) {
			if crsVersion == "" {
				return errUnknownModuleCRSInclude
			}
			return errNoBundledCRSInclude
		}
	}
	if embedsCRS {
		return nil
	}
	// @pmFromFile data under crs/ is served by the same embedded tree; a
	// module without it resolves nothing.
	for _, match := range pmFromFilePattern.FindAllStringSubmatch(active, -1) {
		if len(match) < 3 {
			continue
		}
		target := path.Clean(strings.Trim(strings.TrimSpace(match[2]), `"'`))
		if isEmbeddedDataPath(target) {
			if crsVersion == "" {
				return errUnknownModuleCRSData
			}
			return errNoBundledCRSData
		}
	}
	return nil
}
