package controller

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDataplanePodMonitorMetricRelabelingsDecodeFlatStats(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		flatName    string
		preexisting map[string]string
		wantName    string
		wantLabels  map[string]string
	}{
		{
			name:     "demo engine and hyphenated namespace",
			flatName: "coraza_waf_requests_total_driver_type_wasm_engine_coraza_namespace_integration_tests_outcome_pass",
			wantName: "coraza_waf_requests_total",
			wantLabels: map[string]string{
				"driver_type": "wasm",
				"engine":      "coraza",
				"namespace":   "integration-tests",
				"outcome":     "pass",
			},
		},
		{
			name:     "blocked requests with CRS category",
			flatName: "coraza_waf_blocked_requests_total_category_xss_driver_type_wasm_engine_conformance_engine_namespace_crs_conformance_ad1e41_severity_CRITICAL",
			wantName: "coraza_waf_blocked_requests_total",
			wantLabels: map[string]string{
				"driver_type": "wasm",
				"engine":      "conformance-engine",
				"namespace":   "crs-conformance-ad1e41",
				"category":    "xss",
				"severity":    "CRITICAL",
			},
		},
		{
			name:     "hyphenated engine and namespace from conformance",
			flatName: "coraza_waf_requests_total_driver_type_wasm_engine_conformance_engine_namespace_crs_conformance_ad1e41_outcome_block",
			wantName: "coraza_waf_requests_total",
			wantLabels: map[string]string{
				"driver_type": "wasm",
				"engine":      "conformance-engine",
				"namespace":   "crs-conformance-ad1e41",
				"outcome":     "block",
			},
		},
		{
			name:     "short name from stats_tags keeps preexisting labels",
			flatName: "coraza_waf_requests_total",
			preexisting: map[string]string{
				"driver_type": "wasm",
				"engine":      "coraza",
				"namespace":   "integration-tests",
				"outcome":     "pass",
			},
			wantName: "coraza_waf_requests_total",
			wantLabels: map[string]string{
				"driver_type": "wasm",
				"engine":      "coraza",
				"namespace":   "integration-tests",
				"outcome":     "pass",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotName, gotLabels := applyDataplaneMetricRelabelings(tc.flatName, tc.preexisting)
			assert.Equal(t, tc.wantName, gotName)
			for key, want := range tc.wantLabels {
				assert.Equal(t, want, gotLabels[key], "label %q", key)
			}
		})
	}
}

// applyDataplaneMetricRelabelings simulates Prometheus metricRelabeling replace/keep
// rules on a single series. preexisting labels model stats_tags-exported labels on
// short metric names.
func applyDataplaneMetricRelabelings(flatName string, preexisting map[string]string) (string, map[string]string) {
	labels := map[string]string{"__name__": flatName}
	for k, v := range preexisting {
		labels[k] = v
	}

	for _, rule := range dataplanePodMonitorMetricRelabelings() {
		action, _ := rule["action"].(string)
		switch action {
		case "keep":
			re := regexp.MustCompile(rule["regex"].(string))
			if !re.MatchString(labels["__name__"]) {
				return "", nil
			}
		case "replace":
			re := regexp.MustCompile(rule["regex"].(string))
			targetLabel := rule["targetLabel"].(string)
			replacement := rule["replacement"].(string)
			sourceLabels := toStringSlice(rule["sourceLabels"])

			var sourceValue string
			switch {
			case len(sourceLabels) == 1 && sourceLabels[0] == "__name__":
				sourceValue = labels["__name__"]
			case len(sourceLabels) == 1:
				sourceValue = labels[sourceLabels[0]]
			default:
				continue
			}
			if sourceValue == "" {
				continue
			}

			matches := re.FindStringSubmatch(sourceValue)
			if matches == nil {
				continue
			}
			labels[targetLabel] = expandRelabelReplacement(replacement, matches)
		}
	}

	return labels["__name__"], labels
}

func toStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, len(raw))
	for i, item := range raw {
		out[i], _ = item.(string)
	}
	return out
}

func expandRelabelReplacement(replacement string, matches []string) string {
	out := replacement
	for i := len(matches) - 1; i >= 1; i-- {
		out = strings.ReplaceAll(out, fmt.Sprintf("${%d}", i), matches[i])
		out = strings.ReplaceAll(out, fmt.Sprintf("$%d", i), matches[i])
	}
	return out
}

func TestDataplanePodMonitorMetricRelabelings(t *testing.T) {
	t.Parallel()

	rules := dataplanePodMonitorMetricRelabelings()
	require.GreaterOrEqual(t, len(rules), 48, "expected keep + flat-stat decode + namespace/engine normalize rules")

	assert.Equal(t, "keep", rules[0]["action"])
	assert.Equal(t, "coraza_waf_.*", rules[0]["regex"])
}
