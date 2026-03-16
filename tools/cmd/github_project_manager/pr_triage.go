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
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// -----------------------------------------------------------------------------
// PR triage — area labels
//
// Note: if this becomes unwieldy in the future, consider whether it can be
// wholly or partially placed by https://github.com/actions/labeler.
// -----------------------------------------------------------------------------

var pathAreaRules = []struct {
	label    string
	prefixes []string
}{
	{"area/api", []string{"api/"}},
	{"area/controllers", []string{"internal/controller/"}},
	{"area/cache", []string{"internal/rulesets/"}},
	{"area/testing", []string{"test/", "internal/controller/suite_test.go"}},
	{"area/infrastructure", []string{".github/", "Makefile", "Dockerfile", "hack/", "tools/"}},
	{"area/documentation", []string{"docs/", "README.md", "CONTRIBUTING.md", "DEVELOPMENT.md", "RELEASE.md"}},
	{"area/helm", []string{"charts/"}},
}

// computePRAreaLabels returns area/* labels inferred from changed file paths.
// Skips if the PR already has any area/* label.
func computePRAreaLabels(labels, files []string) []string {
	if hasLabelPrefix(labels, "area/") {
		return nil
	}

	var out []string
	for _, rule := range pathAreaRules {
		if slices.ContainsFunc(files, func(f string) bool {
			return slices.ContainsFunc(rule.prefixes, func(p string) bool {
				return strings.HasPrefix(f, p)
			})
		}) {
			out = append(out, rule.label)
		}
	}

	return out
}

// -----------------------------------------------------------------------------
// PR triage — size labels
// -----------------------------------------------------------------------------

// computePRSizeLabel returns a size/* label based on total lines changed.
// Skips if the PR already has any size/* label.
func computePRSizeLabel(labels []string, additions, deletions int) []string {
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

// -----------------------------------------------------------------------------
// PR triage — milestone selection
// -----------------------------------------------------------------------------

// findLowestMilestone returns the milestone with the lowest semver title,
// or an error if no valid semver milestones exist.
func findLowestMilestone(milestones []Milestone) (*Milestone, error) {
	var best *Milestone
	for i := range milestones {
		m := &milestones[i]
		if _, ok := parseSemver(m.Title); !ok {
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

// -----------------------------------------------------------------------------
// private helpers
// -----------------------------------------------------------------------------

func parseSemver(s string) (*semver.Version, bool) {
	v, err := semver.StrictNewVersion(strings.TrimPrefix(s, "v"))
	if err != nil {
		return nil, false
	}

	return v, true
}

func semverLess(a, b string) bool {
	aVer, aOK := parseSemver(a)
	bVer, bOK := parseSemver(b)
	if !aOK || !bOK {
		return a < b
	}

	return aVer.LessThan(bVer)
}
