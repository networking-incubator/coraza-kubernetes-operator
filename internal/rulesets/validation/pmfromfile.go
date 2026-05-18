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
	"regexp"
	"strings"
)

var pmFromFilePattern = regexp.MustCompile(`(?i)@pmFromFile\s+("([^"]+)"|(\S+))`)

// ValidatePMFromFileData ensures every @pmFromFile reference has a matching
// key in dataFiles (RuleSet-merged RuleData). Coraza may otherwise resolve
// files from the operator process working directory, which is unsafe and
// makes tests environment-dependent.
func ValidatePMFromFileData(aggregatedRules string, dataFiles map[string][]byte) error {
	for _, match := range pmFromFilePattern.FindAllStringSubmatch(aggregatedRules, -1) {
		if len(match) < 2 {
			continue
		}
		basename := strings.Trim(match[1], `"'`)
		if basename == "" {
			continue
		}
		if dataFiles == nil {
			return fmt.Errorf("@pmFromFile %s requires spec.data RuleData but none are referenced", basename)
		}
		if _, ok := dataFiles[basename]; !ok {
			return fmt.Errorf("@pmFromFile %s: file not provided by referenced RuleData", basename)
		}
	}
	return nil
}
