{{/*
Prometheus relabel rules that decode coraza_waf_* labels embedded in flat Envoy stat
names. Required on Istio 1.21+ where EnvoyFilter applyTo: BOOTSTRAP (stats_tags)
no longer patches gateway proxies. When stats_tags already extract labels, __name__
is short (e.g. coraza_waf_requests_total) and these rules no-op.

Engine and namespace values use (.+) bounded by _namespace_ / trailing field anchors
because Envoy encodes K8s hyphens as underscores (conformance-engine -> conformance_engine).
*/}}
{{- define "coraza-operator.podMonitorFlatLabelRelabelings" -}}
- sourceLabels: [__name__]
  regex: 'coraza_waf_.*'
  action: keep
- sourceLabels: [__name__]
  regex: '^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$'
  targetLabel: driver_type
  replacement: '${1}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$'
  targetLabel: engine
  replacement: '${2}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$'
  targetLabel: namespace
  replacement: '${3}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$'
  targetLabel: outcome
  replacement: '${4}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_requests_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)$'
  targetLabel: __name__
  replacement: 'coraza_waf_requests_total'
- sourceLabels: [__name__]
  regex: '^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$'
  targetLabel: category
  replacement: '${1}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$'
  targetLabel: driver_type
  replacement: '${2}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$'
  targetLabel: engine
  replacement: '${3}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$'
  targetLabel: namespace
  replacement: '${4}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$'
  targetLabel: severity
  replacement: '${5}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_blocked_requests_total_category_([^_]+(?:_[^_]+)*)_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_severity_([^_]+)$'
  targetLabel: __name__
  replacement: 'coraza_waf_blocked_requests_total'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$'
  targetLabel: driver_type
  replacement: '${1}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$'
  targetLabel: engine
  replacement: '${2}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$'
  targetLabel: namespace
  replacement: '${3}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$'
  targetLabel: outcome
  replacement: '${4}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$'
  targetLabel: rule_id
  replacement: '${5}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$'
  targetLabel: severity
  replacement: '${6}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_hits_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_outcome_([^_]+)_rule_id_([^_]+)_severity_([^_]+)$'
  targetLabel: __name__
  replacement: 'coraza_waf_rule_hits_total'
- sourceLabels: [__name__]
  regex: '^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$'
  targetLabel: driver_type
  replacement: '${1}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$'
  targetLabel: engine
  replacement: '${2}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$'
  targetLabel: namespace
  replacement: '${3}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$'
  targetLabel: status
  replacement: '${4}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_plugin_loads_total_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_status_([^_]+)$'
  targetLabel: __name__
  replacement: 'coraza_waf_plugin_loads_total'
- sourceLabels: [__name__]
  regex: '^coraza_waf_plugin_rule_count_driver_type_([^_]+)_engine_(.+)_namespace_(.+)$'
  targetLabel: driver_type
  replacement: '${1}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_plugin_rule_count_driver_type_([^_]+)_engine_(.+)_namespace_(.+)$'
  targetLabel: engine
  replacement: '${2}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_plugin_rule_count_driver_type_([^_]+)_engine_(.+)_namespace_(.+)$'
  targetLabel: namespace
  replacement: '${3}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_plugin_rule_count_driver_type_([^_]+)_engine_(.+)_namespace_(.+)$'
  targetLabel: __name__
  replacement: 'coraza_waf_plugin_rule_count'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$'
  targetLabel: driver_type
  replacement: '${1}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$'
  targetLabel: engine
  replacement: '${2}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$'
  targetLabel: namespace
  replacement: '${3}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$'
  targetLabel: rule_id
  replacement: '${4}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$'
  targetLabel: type
  replacement: '${5}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_rule_overrides_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_rule_id_([^_]+)_type_([^_]+(?:_[^_]+)*)$'
  targetLabel: __name__
  replacement: 'coraza_waf_rule_overrides'
- sourceLabels: [__name__]
  regex: '^coraza_waf_request_anomaly_score_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_(bucket|count|sum)$'
  targetLabel: driver_type
  replacement: '${1}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_request_anomaly_score_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_(bucket|count|sum)$'
  targetLabel: engine
  replacement: '${2}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_request_anomaly_score_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_(bucket|count|sum)$'
  targetLabel: namespace
  replacement: '${3}'
- sourceLabels: [__name__]
  regex: '^coraza_waf_request_anomaly_score_driver_type_([^_]+)_engine_(.+)_namespace_(.+)_(bucket|count|sum)$'
  targetLabel: __name__
  replacement: 'coraza_waf_request_anomaly_score_${4}'
{{- end -}}
