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

// Package rulesets provides rule analysis utilities for the operator.
package rulesets

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// -----------------------------------------------------------------------------
// Vars
// -----------------------------------------------------------------------------

// ruleIDPattern matches SecLang rule IDs: "id:12345" or "id:'12345'".
var ruleIDPattern = regexp.MustCompile(`\bid:['"]?(\d+)['"]?`)

// unsupportedRules is the registry of all known unsupported rule IDs.
// Coupled to CoreRuleSet v4.24.1 (source: LIMITATIONS.md, test/conformance/ftw.yml).
var unsupportedRules = buildRegistry()

// -----------------------------------------------------------------------------
// Unsupported Rules Analysis
// -----------------------------------------------------------------------------

// CheckUnsupportedRules scans active (non-comment) rule directives for IDs
// known to be unsupported. Lines starting with # are ignored.
func CheckUnsupportedRules(rules string) []UnsupportedRule {
	active := stripCommentLines(rules)
	matches := ruleIDPattern.FindAllStringSubmatch(active, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[int]bool)
	var found []UnsupportedRule

	for _, match := range matches {
		id, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if seen[id] {
			continue
		}
		if entry, ok := unsupportedRules[id]; ok {
			seen[id] = true
			found = append(found, entry)
		}
	}

	if len(found) == 0 {
		return nil
	}

	sort.Slice(found, func(i, j int) bool {
		return found[i].ID < found[j].ID
	})

	return found
}

// IncompatibleRuleIDs returns all registered incompatible-tier rule IDs.
func IncompatibleRuleIDs() []int {
	return ruleIDsByTier(TierIncompatible)
}

// RedundantRuleIDs returns all registered redundant-tier rule IDs.
func RedundantRuleIDs() []int {
	return ruleIDsByTier(TierRedundant)
}

// FormatUnsupportedMessage returns a human-readable status message for the given unsupported rules.
func FormatUnsupportedMessage(found []UnsupportedRule) string {
	if len(found) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "RuleSet contains %d unsupported rule(s) for the current WASM engine mode:", len(found))
	for _, r := range found {
		fmt.Fprintf(&b, "\n  - rule %d (%s): %s", r.ID, r.Category, r.Description)
	}
	b.WriteString("\nSee LIMITATIONS.md for details and mitigation options.")

	return b.String()
}

// -----------------------------------------------------------------------------
// Types
// -----------------------------------------------------------------------------

// UnsupportedRule describes a single unsupported rule.
type UnsupportedRule struct {
	ID          int
	Category    string
	Tier        Tier
	Description string
}

// Tier classifies the severity of an unsupported rule.
type Tier string

const (
	// TierIncompatible marks rules that do not function in WASM mode (fail silently).
	TierIncompatible Tier = "incompatible"

	// TierRedundant marks rules where Envoy handles the attack case before the WAF.
	TierRedundant Tier = "redundant"
)

// -----------------------------------------------------------------------------
// Registry
// -----------------------------------------------------------------------------

// buildRegistry constructs the unsupported rule registry.
func buildRegistry() map[int]UnsupportedRule {
	r := make(map[int]UnsupportedRule)

	// -------------------------------------------------------------------------
	// Incompatible Tier — rules that do not function in WASM mode
	// -------------------------------------------------------------------------

	// Response body inspection: Envoy does not pass response body to WASM filters.
	for _, id := range []int{
		950150,
		951110, 951120, 951130, 951140, 951150, 951160, 951170, 951180, 951190, 951200, 951210, 951220, 951230, 951240, 951250, 951260,
		952110,
		953101, 953120,
		954100, 954101, 954120,
		955100, 955110, 955120, 955260, 955400,
		956100, 956110,
	} {
		r[id] = UnsupportedRule{
			ID:          id,
			Category:    "response body inspection",
			Tier:        TierIncompatible,
			Description: "response body inspection is not supported in Envoy WASM",
		}
	}

	// Multipart charset detection: Envoy body handling prevents detection.
	r[922110] = UnsupportedRule{
		ID:          922110,
		Category:    "multipart charset detection",
		Tier:        TierIncompatible,
		Description: "multipart charset detection does not function in Envoy/Kubernetes",
	}

	// PL4 false positives: Envoy :path pseudo-header triggers PL4 violations.
	r[920274] = UnsupportedRule{
		ID:          920274,
		Category:    "PL4 false positives",
		Tier:        TierIncompatible,
		Description: "PL4 false positives from Envoy :path pseudo-header population",
	}

	// Coraza/WASM bugs
	r[920171] = UnsupportedRule{
		ID:          920171,
		Category:    "GET/HEAD body detection",
		Tier:        TierIncompatible,
		Description: "GET/HEAD with body not detected (coraza-proxy-wasm bug)",
	}
	r[920430] = UnsupportedRule{
		ID:          920430,
		Category:    "protocol version detection",
		Tier:        TierIncompatible,
		Description: "invalid HTTP protocol version not detected (coraza-proxy-wasm bug)",
	}
	r[934120] = UnsupportedRule{
		ID:          934120,
		Category:    "enclosed alphanumerics HTTP/2",
		Tier:        TierIncompatible,
		Description: "enclosed alphanumerics not detected in HTTP/2 (coraza-proxy-wasm bug)",
	}
	r[932300] = UnsupportedRule{
		ID:          932300,
		Category:    "multiphase evaluation",
		Tier:        TierIncompatible,
		Description: "fails with multiphase evaluation enabled (Coraza bug)",
	}
	r[933120] = UnsupportedRule{
		ID:          933120,
		Category:    "multiphase evaluation",
		Tier:        TierIncompatible,
		Description: "fails with multiphase evaluation enabled (Coraza bug)",
	}

	// -------------------------------------------------------------------------
	// Under Investigation - incompatible rules with unclear cause
	// -------------------------------------------------------------------------

	for _, entry := range []struct {
		id   int
		desc string
	}{
		{920270, "behavioral difference — blocked at different layer"},
		{921140, "behavioral difference — blocked at different layer"},
		{921250, "cookie $Version detection not matching"},
		{922130, "match_regex behavior difference"},
		{932200, "detected by earlier rule instead of expected rule"},
		{941210, "HTML entity decoding — transformation chain issue"},
		{944200, "Java deserialization pattern not detected"},
		{959100, "retry_once mechanism not working in environment"},
		{980170, "retry_once mechanism not working in environment"},
	} {
		r[entry.id] = UnsupportedRule{
			ID:          entry.id,
			Category:    "under investigation",
			Tier:        TierIncompatible,
			Description: entry.desc,
		}
	}

	// -------------------------------------------------------------------------
	// Redundant Tier — rules that work but Envoy handles some cases first
	// -------------------------------------------------------------------------

	// HTTP method/request handling: Envoy rejects invalid methods with 400.
	for _, id := range []int{911100, 920100, 949110} {
		r[id] = UnsupportedRule{
			ID:          id,
			Category:    "HTTP method handling",
			Tier:        TierRedundant,
			Description: "some request patterns handled by Envoy before reaching WAF",
		}
	}

	// Protocol enforcement
	for _, id := range []int{920181, 920210} {
		r[id] = UnsupportedRule{
			ID:          id,
			Category:    "protocol enforcement",
			Tier:        TierRedundant,
			Description: "protocol enforcement handled by Envoy proxy layer",
		}
	}

	// HTTP/2 normalization: Envoy normalizes missing/empty headers.
	for _, id := range []int{920280, 920290, 920300, 920310, 920311, 920320, 920330, 920610} {
		r[id] = UnsupportedRule{
			ID:          id,
			Category:    "HTTP/2 normalization",
			Tier:        TierRedundant,
			Description: "header normalization handled by Envoy in HTTP/2",
		}
	}

	// Header sanitization: Envoy sanitizes malicious payloads in headers.
	for _, id := range []int{932161, 932207, 932237, 932239, 941101, 941110, 941120, 942280} {
		r[id] = UnsupportedRule{
			ID:          id,
			Category:    "header sanitization",
			Tier:        TierRedundant,
			Description: "malicious header payloads sanitized by Envoy before reaching WAF",
		}
	}

	return r
}

// -----------------------------------------------------------------------------
// Private Utilities
// -----------------------------------------------------------------------------

// ruleIDsByTier returns sorted rule IDs for the given tier.
func ruleIDsByTier(tier Tier) []int {
	var ids []int
	for id, rule := range unsupportedRules {
		if rule.Tier == tier {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)

	return ids
}

// stripCommentLines returns rules with comment-only lines removed.
// SecLang comments are lines whose first non-whitespace character is #.
func stripCommentLines(rules string) string {
	var b strings.Builder
	for _, line := range strings.Split(rules, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] == '#' {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
