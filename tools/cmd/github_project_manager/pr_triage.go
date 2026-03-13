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

package main

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// pathAreaRule maps file path prefixes to area labels.
type pathAreaRule struct {
	label    string
	prefixes []string
}

var pathAreaRules = []pathAreaRule{
	{"area/api", []string{"api/"}},
	{"area/controllers", []string{"internal/controller/"}},
	{"area/cache", []string{"internal/rulesets/"}},
	{"area/testing", []string{"test/", "internal/controller/suite_test.go"}},
	{"area/infrastructure", []string{".github/", "Makefile", "Dockerfile", "hack/", "tools/"}},
	{"area/documentation", []string{"docs/", "README.md", "CONTRIBUTING.md", "DEVELOPMENT.md", "RELEASE.md"}},
	{"area/helm", []string{"charts/"}},
}

// ComputePRAreaLabels returns area/* labels inferred from changed file paths.
// Skips if the PR already has any area/* label.
func ComputePRAreaLabels(labels, files []string) []string {
	if hasLabelPrefix(labels, "area/") {
		return nil
	}

	seen := make(map[string]bool)
	var out []string
	for _, f := range files {
		for _, rule := range pathAreaRules {
			if seen[rule.label] {
				continue
			}
			for _, prefix := range rule.prefixes {
				if strings.HasPrefix(f, prefix) || f == prefix {
					seen[rule.label] = true
					out = append(out, rule.label)
					break
				}
			}
		}
	}

	return out
}

// ComputePRSizeLabel returns a size/* label based on total lines changed.
// Skips if the PR already has any size/* label.
func ComputePRSizeLabel(labels []string, additions, deletions int) []string {
	if hasLabelPrefix(labels, "size/") {
		return nil
	}

	total := additions + deletions
	var label string
	switch {
	case total <= 10:
		label = "size/XS"
	case total <= 50:
		label = "size/S"
	case total <= 200:
		label = "size/M"
	case total <= 500:
		label = "size/L"
	default:
		label = "size/XL"
	}

	return []string{label}
}

// semverParts parses a semver string like "1.2.3" (with optional "v" prefix)
// into a semver.Version. Returns ok=false if parsing fails.
func semverParts(s string) (*semver.Version, bool) {
	v, err := semver.StrictNewVersion(strings.TrimPrefix(s, "v"))
	if err != nil {
		return nil, false
	}
	return v, true
}

// semverLess returns true if a < b in semver ordering.
func semverLess(a, b string) bool {
	aVer, aOK := semverParts(a)
	bVer, bOK := semverParts(b)
	if !aOK || !bOK {
		return a < b // fallback to lexicographic
	}
	return aVer.LessThan(bVer)
}

// FindLowestMilestone returns the milestone with the lowest semver title,
// or an error if no valid semver milestones exist.
func FindLowestMilestone(milestones []Milestone) (*Milestone, error) {
	var best *Milestone
	for i := range milestones {
		m := &milestones[i]
		if _, ok := semverParts(m.Title); !ok {
			continue
		}
		if best == nil || semverLess(m.Title, best.Title) {
			best = m
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no open milestones with semver titles found")
	}
	return best, nil
}
