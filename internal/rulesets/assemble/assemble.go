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

// Package assemble builds deterministic RuleSet content from resolved inputs.
// It performs no Kubernetes I/O: callers resolve RuleSource and RuleData
// objects and pass their contents in.
package assemble

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
)

// ErrEmptyRules is returned when the assembled rule text is empty and the
// RuleSet's empty rules policy is Reject. An empty RuleSet must never become a
// pass-through WAF by accident.
var ErrEmptyRules = errors.New("assembled rules are empty")

// EmptyRulesPolicy controls how Assemble treats empty rule text.
type EmptyRulesPolicy string

// maxDataFileNameBytes mirrors the plugin's data file name cap.
const maxDataFileNameBytes = 253

// maxReportedDataFiles bounds the invalid-name enumeration in status messages,
// so a legacy object with thousands of bad names cannot overflow them.
const maxReportedDataFiles = 5

// maxReportedDataFileNameBytes bounds each quoted name, so one legacy
// multi-megabyte key cannot overflow the condition or readiness text.
const maxReportedDataFileNameBytes = 64

// embeddedDataFiles are served by the plugin's embedded filesystem. That tree
// is merged first, so a RuleData file with one of these names would be shadowed
// and silently ignored: validateReferences rejects it instead.
var embeddedDataFiles = map[string]struct{}{
	"coraza.conf-recommended.conf": {},
	"coraza-demo.conf":             {},
	"ftw-config.conf":              {},
	"crs-setup.conf.example":       {},
	// The plugin's include aliases resolve to the files above.
	"@recommended-conf": {},
	"@demo-conf":        {},
	"@ftw-conf":         {},
	"@crs-setup-conf":   {},
	"@owasp_crs":        {},
}

const (
	// EmptyRulesPolicyReject rejects empty assembled rules. It is the default.
	EmptyRulesPolicyReject EmptyRulesPolicy = "Reject"
	// EmptyRulesPolicyAllow accepts empty assembled rules as an explicit no-op WAF.
	EmptyRulesPolicyAllow EmptyRulesPolicy = "Allow"
)

// Content is the deterministic assembled RuleSet content.
type Content struct {
	Rules     string
	DataFiles map[string][]byte
}

// Inputs are the resolved inputs for one RuleSet. Sources and Data preserve
// the order declared in the RuleSet spec.
type Inputs struct {
	RuleSetUID       types.UID
	RuleSetName      string
	Namespace        string
	EmptyRulesPolicy EmptyRulesPolicy
	Sources          []SourceInput
	Data             []DataInput
}

// SourceInput is one resolved RuleSource referenced by a RuleSet.
type SourceInput struct {
	Name       string
	UID        types.UID
	Generation int64
	Rules      string
}

// DataInput is one resolved RuleData referenced by a RuleSet.
type DataInput struct {
	Name       string
	UID        types.UID
	Generation int64
	Files      map[string]string
}

// Assemble concatenates source rules in list order, merges data files with
// last-listed-wins semantics, rejects duplicate references, validates
// @pmFromFile references, and applies the empty rules policy.
func Assemble(in Inputs) (Content, error) {
	if err := validateReferences(in); err != nil {
		return Content{}, err
	}

	var rules strings.Builder
	for i, src := range in.Sources {
		rules.WriteString(src.Rules)
		if i < len(in.Sources)-1 {
			rules.WriteString("\n")
		}
	}

	var dataFiles map[string][]byte
	if len(in.Data) > 0 {
		dataFiles = make(map[string][]byte)
		for _, data := range in.Data {
			for name, content := range data.Files {
				dataFiles[name] = []byte(content)
			}
		}
	}

	assembled := rules.String()
	if err := validation.ValidatePMFromFileData(assembled, dataFiles); err != nil {
		return Content{}, err
	}

	switch policy := in.EmptyRulesPolicy; policy {
	case "", EmptyRulesPolicyReject:
		if strings.TrimSpace(assembled) == "" {
			return Content{}, fmt.Errorf("%w: RuleSet %s/%s has no rules", ErrEmptyRules, in.Namespace, in.RuleSetName)
		}
	case EmptyRulesPolicyAllow:
	default:
		return Content{}, fmt.Errorf("unknown empty rules policy %q", policy)
	}

	return Content{Rules: assembled, DataFiles: dataFiles}, nil
}

// inputIdentity is the resolvable identity of one RuleSet input object.
type inputIdentity struct {
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

// Fingerprint returns a deterministic identity of the resolved inputs,
// including object UIDs and generations. It changes when inputs are
// re-resolved even if the effective content does not; use
// render.RuleSetContentHash for content identity.
func Fingerprint(in Inputs) string {
	// Hash only the input identities, never the rule corpus: this runs on
	// every publication reconcile, before coalescing can short-circuit, and
	// the corpus can reach the multi-megabyte message budget.
	sources := make([]inputIdentity, len(in.Sources))
	for i, src := range in.Sources {
		sources[i] = inputIdentity{Name: src.Name, UID: src.UID, Generation: src.Generation}
	}
	data := make([]inputIdentity, len(in.Data))
	for i, d := range in.Data {
		data[i] = inputIdentity{Name: d.Name, UID: d.UID, Generation: d.Generation}
	}
	raw, err := json.Marshal(struct {
		RuleSetUID       types.UID        `json:"rule_set_uid"`
		RuleSetName      string           `json:"rule_set_name"`
		Namespace        string           `json:"namespace"`
		EmptyRulesPolicy EmptyRulesPolicy `json:"empty_rules_policy"`
		Sources          []inputIdentity  `json:"sources"`
		Data             []inputIdentity  `json:"data"`
	}{
		RuleSetUID:       in.RuleSetUID,
		RuleSetName:      in.RuleSetName,
		Namespace:        in.Namespace,
		EmptyRulesPolicy: in.EmptyRulesPolicy,
		Sources:          sources,
		Data:             data,
	})
	if err != nil {
		// The identity carries only strings and integers; marshal cannot
		// fail. Panic rather than let every Engine share one constant
		// fingerprint and silently freeze publication cluster-wide.
		panic(fmt.Sprintf("assemble: marshal input identities: %v", err))
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateReferences(in Inputs) error {
	var msgs []string

	if dups := duplicateNames(in.Sources, func(s SourceInput) string { return s.Name }); len(dups) > 0 {
		msgs = append(msgs, fmt.Sprintf("spec.sources contains duplicate RuleSource name(s): %s", strings.Join(dups, ", ")))
	}

	if dups := duplicateNames(in.Data, func(d DataInput) string { return d.Name }); len(dups) > 0 {
		msgs = append(msgs, fmt.Sprintf("spec.data contains duplicate RuleData name(s): %s", strings.Join(dups, ", ")))
	}

	// The plugin resolves every data file name with fs.ValidPath, rejects ".",
	// and rejects names longer than 253 bytes, failing the whole configuration.
	// CEL only guards writes, so objects created before those rules existed
	// must be caught here. A name with "/" is also rejected: the CRD's CEL
	// pattern has no separator, and Coraza resolves a nested Include relative
	// to the including file's directory, so a generated directory would make
	// the guard scan a different file than the plugin parses.
	var invalid, shadowed []string
	for _, data := range in.Data {
		for name := range data.Files {
			switch {
			case name == "." || len(name) > maxDataFileNameBytes || strings.Contains(name, "/") || !fs.ValidPath(name):
				invalid = append(invalid, fmt.Sprintf("%q (RuleData %s)", truncateDataFileName(name), data.Name))
			case shadowsEmbeddedFile(name):
				shadowed = append(shadowed, fmt.Sprintf("%q (RuleData %s)", truncateDataFileName(name), data.Name))
			}
		}
	}
	if len(invalid) > 0 {
		sort.Strings(invalid)
		msgs = append(msgs, fmt.Sprintf("spec.data contains invalid data file name(s): %s", summarizeDataFiles(invalid)))
	}
	if len(shadowed) > 0 {
		sort.Strings(shadowed)
		msgs = append(msgs, fmt.Sprintf("spec.data contains data file name(s) that shadow embedded plugin files: %s", summarizeDataFiles(shadowed)))
	}

	if len(msgs) > 0 {
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
}

// shadowsEmbeddedFile reports whether name resolves to an embedded plugin file
// before the merged RuleData filesystem is consulted.
func shadowsEmbeddedFile(name string) bool {
	if _, ok := embeddedDataFiles[name]; ok {
		return true
	}
	return name == "crs" || strings.HasPrefix(name, "crs/") ||
		name == "@owasp_crs" || strings.HasPrefix(name, "@owasp_crs/")
}

// truncateDataFileName keeps status messages bounded for legacy keys that
// predate the API's 253-byte name cap.
func truncateDataFileName(name string) string {
	if len(name) <= maxReportedDataFileNameBytes {
		return name
	}
	return name[:maxReportedDataFileNameBytes] + "..."
}

func summarizeDataFiles(entries []string) string {
	if len(entries) <= maxReportedDataFiles {
		return strings.Join(entries, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(entries[:maxReportedDataFiles], ", "), len(entries)-maxReportedDataFiles)
}

func duplicateNames[T any](items []T, name func(T) string) []string {
	seen := make(map[string]int, len(items))
	for _, item := range items {
		seen[name(item)]++
	}

	var dups []string
	for n, count := range seen {
		if count > 1 {
			dups = append(dups, n)
		}
	}
	sort.Strings(dups)
	return dups
}
