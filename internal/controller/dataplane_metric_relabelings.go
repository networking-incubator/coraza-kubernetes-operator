package controller

func dataplanePodMonitorMetricRelabelings() []map[string]any {
	rules := []map[string]any{
		metricRelabelKeep("coraza_waf_.*"),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$`, "driver_type", `${1}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$`, "engine", `${2}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$`, "namespace", `${3}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$`, "outcome", `${4}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$`, "__name__", `coraza_waf_requests_total`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$`, "category", `${1}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$`, "driver_type", `${2}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$`, "engine", `${3}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$`, "namespace", `${4}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$`, "severity", `${5}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$`, "__name__", `coraza_waf_blocked_requests_total`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$`, "driver_type", `${1}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$`, "engine", `${2}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$`, "namespace", `${3}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$`, "outcome", `${4}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$`, "rule_id", `${5}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$`, "severity", `${6}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$`, "__name__", `coraza_waf_rule_hits_total`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$`, "driver_type", `${1}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$`, "engine", `${2}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$`, "namespace", `${3}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$`, "status", `${4}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$`, "__name__", `coraza_waf_plugin_loads_total`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_plugin_rule_count_driver_type_([^_]+)_engine_(.+)_namespace_(.+)$`, "driver_type", `${1}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_plugin_rule_count_driver_type_([^_]+)_engine_(.+)_namespace_(.+)$`, "engine", `${2}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_plugin_rule_count_driver_type_([^_]+)_engine_(.+)_namespace_(.+)$`, "namespace", `${3}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_plugin_rule_count_driver_type_([^_]+)_engine_(.+)_namespace_(.+)$`, "__name__", `coraza_waf_plugin_rule_count`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$`, "driver_type", `${1}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$`, "engine", `${2}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$`, "namespace", `${3}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$`, "rule_id", `${4}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$`, "type", `${5}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$`, "__name__", `coraza_waf_rule_overrides`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_request_anomaly_score_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_(bucket|count|sum)$`, "driver_type", `${1}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_request_anomaly_score_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_(bucket|count|sum)$`, "engine", `${2}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_request_anomaly_score_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_(bucket|count|sum)$`, "namespace", `${3}`),
		metricRelabelReplace([]string{"__name__"}, `^coraza_waf_request_anomaly_score_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_(bucket|count|sum)$`, "__name__", `coraza_waf_request_anomaly_score_${4}`),
	}
	for range 5 {
		rules = append(rules, metricRelabelReplace([]string{"namespace"}, `([^_]+)_(.+)`, "namespace", "${1}-${2}"))
	}
	// Envoy encodes K8s hyphens as underscores in flat stat names (conformance-engine ->
	// conformance_engine). Same one-hyphen-per-pass normalization as namespace above.
	for range 5 {
		rules = append(rules, metricRelabelReplace([]string{"engine"}, `([^_]+)_(.+)`, "engine", "${1}-${2}"))
	}
	return rules
}

func metricRelabelKeep(regex string) map[string]any {
	return map[string]any{
		"sourceLabels": []any{"__name__"},
		"regex":        regex,
		"action":       "keep",
	}
}

func metricRelabelReplace(sourceLabels []string, regex, targetLabel, replacement string) map[string]any {
	labels := make([]any, len(sourceLabels))
	for i, label := range sourceLabels {
		labels[i] = label
	}
	return map[string]any{
		"sourceLabels": labels,
		"regex":        regex,
		"targetLabel":  targetLabel,
		"replacement":  replacement,
		"action":       "replace",
	}
}
