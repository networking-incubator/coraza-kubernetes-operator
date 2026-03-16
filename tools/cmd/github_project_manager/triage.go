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
	"slices"
	"strings"
)

// -----------------------------------------------------------------------------
// Issue triage — label updates
// -----------------------------------------------------------------------------

// triageResult holds the changes to apply to an issue.
type triageResult struct {
	LabelsToAdd    []string
	LabelsToRemove []string
}

// computeLabelUpdates determines label changes based on milestone status.
//
// Rules:
//  1. If no milestone and no triage label: add "triage/needs-triage".
//  2. If no milestone and "triage/accepted" present: remove it, and add "triage/needs-triage".
//  3. If no milestone and another triage label exists alongside
//     "triage/needs-triage" (except "triage/accepted"): remove "triage/needs-triage".
//  4. If milestone present: ensure "triage/accepted", remove other triage labels.
func computeLabelUpdates(labels []string, hasMilestone bool) triageResult {
	var result triageResult

	if !hasMilestone {
		// Remove triage/accepted when there's no milestone
		if slices.Contains(labels, "triage/accepted") {
			result.LabelsToRemove = append(result.LabelsToRemove, "triage/accepted")
		}

		// Count remaining triage labels (excluding triage/accepted which we're removing)
		var triageCount int
		for _, l := range labels {
			if strings.HasPrefix(l, "triage/") && l != "triage/accepted" {
				triageCount++
			}
		}

		if triageCount == 0 {
			result.LabelsToAdd = append(result.LabelsToAdd, "triage/needs-triage")
		} else if slices.Contains(labels, "triage/needs-triage") && triageCount > 1 {
			// Another triage label exists alongside needs-triage
			result.LabelsToRemove = append(result.LabelsToRemove, "triage/needs-triage")
		}
	} else {
		// Has milestone: ensure triage/accepted, remove others
		if !slices.Contains(labels, "triage/accepted") {
			result.LabelsToAdd = append(result.LabelsToAdd, "triage/accepted")
		}

		for _, l := range labels {
			if strings.HasPrefix(l, "triage/") && l != "triage/accepted" {
				result.LabelsToRemove = append(result.LabelsToRemove, l)
			}
		}
	}

	return result
}

// -----------------------------------------------------------------------------
// Issue triage — declined handling
// -----------------------------------------------------------------------------

// declinedResult holds the changes to apply when an issue is declined.
type declinedResult struct {
	LabelsToRemove  []string
	RemoveMilestone bool
	CloseIssue      bool
}

// computeDeclined determines changes for a declined issue.
//
// If the issue has "triage/declined":
//   - Remove all other triage/* labels.
//   - Remove milestone if present.
//   - Close the issue if it's open.
//
// Returns nil if the issue is not declined.
func computeDeclined(labels []string, hasMilestone bool, state string) *declinedResult {
	if !slices.Contains(labels, "triage/declined") {
		return nil
	}

	result := &declinedResult{
		RemoveMilestone: hasMilestone,
		CloseIssue:      state != "closed",
	}

	for _, l := range labels {
		if strings.HasPrefix(l, "triage/") && l != "triage/declined" {
			result.LabelsToRemove = append(result.LabelsToRemove, l)
		}
	}

	return result
}

// -----------------------------------------------------------------------------
// Issue triage — area and size labels
// -----------------------------------------------------------------------------

var areaRules = []struct {
	label    string
	keywords []string
}{
	{"area/testing", []string{"test", "testing", "e2e", "unit test", "integration test"}},
	{"area/infrastructure", []string{"ci", "pipeline", "build", "makefile", "dockerfile", "github action", "workflow", "script"}},
	{"area/documentation", []string{"docs", "documentation", "readme", "guide"}},
	{"area/refinements", []string{"refactor", "improvement", "cleanup", "enhance", "technical debt"}},
	{"area/perfscale", []string{"performance", "scale", "scaling", "latency", "throughput", "benchmark"}},
}

// computeAreaLabels returns area/* labels inferred from the issue body.
// Only runs for triage/accepted issues with no existing area/* label.
func computeAreaLabels(labels []string, body string) []string {
	if !slices.Contains(labels, "triage/accepted") || hasLabelPrefix(labels, "area/") {
		return nil
	}

	lower := strings.ToLower(body)
	var out []string
	for _, r := range areaRules {
		if slices.ContainsFunc(r.keywords, func(kw string) bool {
			return strings.Contains(lower, kw)
		}) {
			out = append(out, r.label)
		}
	}

	return out
}

// computeSizeLabels returns "size/needs-sizing" when the issue is
// triage/accepted but has no size/* label.
func computeSizeLabels(labels []string) []string {
	if slices.Contains(labels, "triage/accepted") && !hasLabelPrefix(labels, "size/") {
		return []string{"size/needs-sizing"}
	}

	return nil
}

// -----------------------------------------------------------------------------
// private helpers
// -----------------------------------------------------------------------------

// effectiveLabels returns labels as they would be after applying a triageResult.
func effectiveLabels(labels []string, result triageResult) []string {
	out := slices.DeleteFunc(slices.Clone(labels), func(l string) bool {
		return slices.Contains(result.LabelsToRemove, l)
	})

	return append(out, result.LabelsToAdd...)
}
