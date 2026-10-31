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
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

// -----------------------------------------------------------------------------
// Bundled CRS capability
// -----------------------------------------------------------------------------

// BundledCRSVersion is the OWASP CRS release embedded in the bundled
// envoy-coraza module, or "none" when the module was built without the
// embed-crs feature. It defaults to the bundled manifest's crs_version;
// tests override it to simulate a CRS-enabled module. crs/ Include targets
// and @pmFromFile data files resolve only in such a module: anything else
// compiles a zero-rule WAF while the operator reports Ready, so they are
// rejected instead (errNoBundledCRSInclude, errNoBundledCRSData).
var BundledCRSVersion = manifestCRSVersion()

func manifestCRSVersion() string {
	manifest, _, err := assets.ResolveDefault()
	if err != nil {
		return "none"
	}
	return manifest.CRSVersion
}

// moduleEmbedsCRS reports whether the bundled module can resolve crs/...
// references from its embedded CRS tree.
func moduleEmbedsCRS() bool {
	return BundledCRSVersion != "" && BundledCRSVersion != "none"
}

// The messages are static so tenant rule content cannot leak into status.
var (
	errNoBundledCRSInclude = errors.New("include of crs/... is not allowed: the bundled module does not include OWASP CRS (crs_version: none); use a module built with `make wasm.crs` or remove the crs/ include")
	errNoBundledCRSData    = errors.New("@pmFromFile of crs/... is not allowed: the bundled module does not include OWASP CRS (crs_version: none); use a module built with `make wasm.crs` or remove the crs/ data reference")
)

// -----------------------------------------------------------------------------
// CRS tuning by rule ID
// -----------------------------------------------------------------------------

// crsPatchByIDPattern matches the configure-time patch directives that address
// a rule by ID, and the ID they address. The Remove* and ByTag/ByMsg forms do
// not fail on an unknown target, so only these two need a stand-in rule.
var crsPatchByIDPattern = regexp.MustCompile(`(?im)^secrule(?:updatetarget|updateaction)byid[ \t]+(\d+)`)

// ruleIDActionPattern matches an id action in every form Coraza's action
// parser accepts (id:1, id: 1, id:'1'). shortcut: id-like text in a pattern
// or message also counts as defined; the pairing with a tuning directive for
// that exact ID is too contrived to matter.
var ruleIDActionPattern = regexp.MustCompile(`(?i)\bid:[ \t]*'?(\d+)`)

// crsTuningStandInRules returns a SecRule definition for every rule ID a
// SecRuleUpdateTargetById or SecRuleUpdateActionById directive addresses
// without a definition in rules or in the RuleData files it Includes. The
// validation WAF compiles crs/ includes against empty stubs (BuildWAFConfig),
// so tuning a CRS rule by ID would otherwise fail with "rule not found"
// although the CRS-enabled plugin resolves the ID from its embedded tree; the
// stand-in lets Coraza still validate the directive's syntax. Rule text whose
// include set has no crs/ target returns "", so an unknown rule ID keeps
// failing validation there.
func crsTuningStandInRules(rules string, dataFiles map[string][]byte) string {
	crsIncluded := false
	referenced := map[string]struct{}{}
	defined := map[string]struct{}{}
	seen := make(map[string]struct{}, len(dataFiles))
	queue := []struct{ name, text string }{{text: rules}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if !crsIncluded && len(crsIncludeTargets(current.text)) > 0 {
			crsIncluded = true
		}
		active := joinActiveRuleLines(current.text)
		for _, match := range crsPatchByIDPattern.FindAllStringSubmatch(active, -1) {
			referenced[match[1]] = struct{}{}
		}
		for _, match := range ruleIDActionPattern.FindAllStringSubmatch(active, -1) {
			defined[match[1]] = struct{}{}
		}
		for _, name := range ruleDataIncludeNames(current.text) {
			name = path.Join(path.Dir(current.name), name)
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
	if !crsIncluded {
		return ""
	}
	// Keep the generated text stable: the validation WAF is compiled from it.
	ids := make([]string, 0, len(referenced))
	for id := range referenced {
		if _, ok := defined[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		// A phase-1 pass rule accepts every target and action update a
		// tenant can address by ID.
		fmt.Fprintf(&b, "SecRule ARGS \"@rx .\" \"id:%s,phase:1,pass,nolog\"\n", id)
	}
	return b.String()
}
