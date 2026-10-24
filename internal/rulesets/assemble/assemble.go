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

// Fingerprint returns a deterministic identity of the resolved inputs,
// including object UIDs and generations. It changes when inputs are
// re-resolved even if the effective content does not; use
// render.RuleSetContentHash for content identity.
func Fingerprint(in Inputs) string {
	// Inputs contains only JSON-marshalable types; json.Marshal cannot fail.
	raw, _ := json.Marshal(in)
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

	if len(msgs) > 0 {
		return errors.New(strings.Join(msgs, "; "))
	}
	return nil
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
