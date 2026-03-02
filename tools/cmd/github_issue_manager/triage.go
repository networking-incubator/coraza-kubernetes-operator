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

import "strings"

// TriageResult holds the changes to apply to an issue.
type TriageResult struct {
	LabelsToAdd    []string
	LabelsToRemove []string
}

// ComputeLabelUpdates determines label changes based on milestone status.
//
// Rules:
//  1. If no milestone and no triage label: add "triage/needs-triage".
//  2. If no milestone and "triage/accepted" present: remove it, and add "triage/needs-triage".
//  3. If no milestone and another triage label exists alongside
//     "triage/needs-triage" (except "triage/accepted"): remove "triage/needs-triage".
//  4. If milestone present: ensure "triage/accepted", remove other triage labels.
func ComputeLabelUpdates(labels []string, hasMilestone bool) TriageResult {
	var result TriageResult

	if !hasMilestone {
		// Remove triage/accepted when there's no milestone
		if contains(labels, "triage/accepted") {
			result.LabelsToRemove = append(result.LabelsToRemove, "triage/accepted")
		}

		// Count remaining triage labels (excluding triage/accepted which we're removing)
		remaining := filter(labels, func(l string) bool {
			return strings.HasPrefix(l, "triage/") && l != "triage/accepted"
		})

		if len(remaining) == 0 {
			result.LabelsToAdd = append(result.LabelsToAdd, "triage/needs-triage")
		} else if contains(labels, "triage/needs-triage") && len(remaining) > 1 {
			// Another triage label exists alongside needs-triage
			result.LabelsToRemove = append(result.LabelsToRemove, "triage/needs-triage")
		}
	} else {
		// Has milestone: ensure triage/accepted, remove others
		if !contains(labels, "triage/accepted") {
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

// DeclinedResult holds the changes to apply when an issue is declined.
type DeclinedResult struct {
	LabelsToRemove  []string
	RemoveMilestone bool
	CloseIssue      bool
}

// ComputeDeclined determines changes for a declined issue.
//
// If the issue has "triage/declined":
//   - Remove all other triage/* labels.
//   - Remove milestone if present.
//   - Close the issue if it's open.
//
// Returns nil if the issue is not declined.
func ComputeDeclined(labels []string, hasMilestone bool, state string) *DeclinedResult {
	if !contains(labels, "triage/declined") {
		return nil
	}

	result := &DeclinedResult{
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

// areaRule maps keywords to an area label.
type areaRule struct {
	label    string
	keywords []string
}

var areaRules = []areaRule{
	{"area/testing", []string{"test", "testing", "e2e", "unit test", "integration test"}},
	{"area/infrastructure", []string{"ci", "pipeline", "build", "makefile", "dockerfile", "github action", "workflow", "script"}},
	{"area/documentation", []string{"docs", "documentation", "readme", "guide"}},
	{"area/refinements", []string{"refactor", "improvement", "cleanup", "enhance", "technical debt"}},
	{"area/perfscale", []string{"performance", "scale", "scaling", "latency", "throughput", "benchmark"}},
}

// ComputeSizeLabels returns "size/needs-sizing" when the issue is
// triage/accepted but has no size/* label.
func ComputeSizeLabels(labels []string) []string {
	if contains(labels, "triage/accepted") && !hasLabelPrefix(labels, "size/") {
		return []string{"size/needs-sizing"}
	}
	return nil
}

// ComputeAreaLabels returns area/* labels inferred from the issue body.
// Only runs for triage/accepted issues with no existing area/* label.
func ComputeAreaLabels(labels []string, body string) []string {
	if !contains(labels, "triage/accepted") || hasLabelPrefix(labels, "area/") {
		return nil
	}

	lower := strings.ToLower(body)
	var out []string
	for _, r := range areaRules {
		for _, kw := range r.keywords {
			if strings.Contains(lower, kw) {
				out = append(out, r.label)
				break
			}
		}
	}
	return out
}

// effectiveLabels returns labels as they would be after applying a TriageResult.
func effectiveLabels(labels []string, result TriageResult) []string {
	out := filter(labels, func(l string) bool { return !contains(result.LabelsToRemove, l) })
	return append(out, result.LabelsToAdd...)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func hasLabelPrefix(labels []string, prefix string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func filter(ss []string, fn func(string) bool) []string {
	var out []string
	for _, s := range ss {
		if fn(s) {
			out = append(out, s)
		}
	}
	return out
}
