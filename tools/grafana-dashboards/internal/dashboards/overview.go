package dashboards

import (
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/text"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/panels"
	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/prom"
)

type healthStat struct {
	title       string
	expr        string
	description string
	unit        string
	steps       []panels.ThresholdStep
}

// BuildOverview generates the full Overview dashboard.
func BuildOverview() (dashboard.Dashboard, error) {
	builder := overviewBaseBuilder()
	addOverviewIntro(builder)
	addHealthSummary(builder)
	addReconciliation(builder)
	addValidation(builder)
	addCacheRED(builder)
	addCacheUSE(builder)
	addKubernetesAPI(builder)
	addResourceCounts(builder)
	return builder.Build()
}

func addOverviewIntro(builder *dashboard.DashboardBuilder) {
	builder.WithPanel(text.NewPanelBuilder().
		Id(1).
		Title("").
		GridPos(dashboard.GridPos{H: 3, W: 24, X: 0, Y: 0}).
		Mode(text.TextModeMarkdown).
		Content(
			"Control-plane health for the Coraza Kubernetes Operator. " +
				"Use **Resource drill-down** (top-right link) for per-CR troubleshooting. " +
				"Alerts are defined in the Helm `PrometheusRule` when enabled.",
		),
	)
}

func addHealthSummary(builder *dashboard.DashboardBuilder) {
	builder.WithRow(dashboard.NewRowBuilder("Health summary").
		Id(2).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: 3}),
	)

	health := []healthStat{
		{
			title:       "Engines not ready",
			expr:        "coraza:engines_not_ready:count",
			description: "Recording rule from PrometheusRule. Count of Engines where Ready != True.",
			unit:        "none",
			steps:       []panels.ThresholdStep{{Color: "green"}, {Value: floatPtr(1), Color: "red"}},
		},
		{
			title:       "RuleSets not ready",
			expr:        "coraza:rulesets_not_ready:count",
			description: "Recording rule from PrometheusRule.",
			unit:        "none",
			steps:       []panels.ThresholdStep{{Color: "green"}, {Value: floatPtr(1), Color: "red"}},
		},
		{
			title:       "RuleSources degraded",
			expr:        "coraza:rulesources_degraded:count",
			description: "Recording rule from PrometheusRule. RuleSources in Degraded condition.",
			unit:        "none",
			steps: []panels.ThresholdStep{
				{Color: "green"},
				{Value: floatPtr(1), Color: "yellow"},
				{Value: floatPtr(2), Color: "red"},
			},
		},
		{
			title: "Reconcile error ratio",
			expr: "(sum(rate(controller_runtime_reconcile_errors_total" +
				"{controller=~\"ruleset|engine|rulesource\"}[5m])) / " +
				"sum(rate(controller_runtime_reconcile_total" +
				"{controller=~\"ruleset|engine|rulesource\"}[5m]))) " +
				"and sum(rate(controller_runtime_reconcile_total" +
				"{controller=~\"ruleset|engine|rulesource\"}[5m])) > 0",
			description: "Ratio of reconcile errors to total reconciles (5m rate).",
			unit:        "percentunit",
			steps: []panels.ThresholdStep{
				{Color: "green"},
				{Value: floatPtr(0.01), Color: "yellow"},
				{Value: floatPtr(0.1), Color: "red"},
			},
		},
		{
			title: "Cache utilization",
			expr: "(coraza_cache_size_bytes / coraza_cache_config_max_size_bytes) " +
				"and coraza_cache_config_max_size_bytes > 0",
			description: "Cache size vs configured maximum.",
			unit:        "percentunit",
			steps: []panels.ThresholdStep{
				{Color: "green"},
				{Value: floatPtr(0.7), Color: "yellow"},
				{Value: floatPtr(0.9), Color: "red"},
			},
		},
		{
			title:       "Cache hit ratio",
			expr:        "coraza:cache_hit_ratio:rate5m",
			description: "Recording rule: 200s / (200s + 404s) over 5m. Shows no data when there is no cache traffic.",
			unit:        "percentunit",
			steps: []panels.ThresholdStep{
				{Color: "red"},
				{Value: floatPtr(0.5), Color: "yellow"},
				{Value: floatPtr(0.8), Color: "green"},
			},
		},
	}

	const healthRowY uint32 = 4
	for i, stat := range health {
		builder.WithPanel(panels.StatPanel(panels.StatPanelConfig{
			ID:          uint32(3 + i),
			Title:       stat.title,
			Description: stat.description,
			Expr:        stat.expr,
			Unit:        stat.unit,
			Steps:       stat.steps,
			GridPos: dashboard.GridPos{
				H: 4,
				W: 4,
				X: uint32(i * 4),
				Y: healthRowY,
			},
		}))
	}
}

func addReconciliation(builder *dashboard.DashboardBuilder) {
	const y uint32 = 8
	builder.WithRow(dashboard.NewRowBuilder("Reconciliation").
		Id(9).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: y}),
	)

	controllerFilter := `{controller=~"ruleset|engine|rulesource"}`

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          10,
		Title:       "Reconcile rate",
		Description: "Reconciles per second by controller and result.",
		Unit:        "ops",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 1},
		Queries: []prom.QuerySpec{{
			Expr:   `sum by (controller, result) (rate(controller_runtime_reconcile_total` + controllerFilter + `[5m]))`,
			Legend: "{{controller}} {{result}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:      11,
		Title:   "Reconcile errors",
		Unit:    "ops",
		GridPos: dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 1},
		Queries: []prom.QuerySpec{{
			Expr:   `sum by (controller) (rate(controller_runtime_reconcile_errors_total` + controllerFilter + `[5m]))`,
			Legend: "{{controller}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:      12,
		Title:   "Reconcile duration p99",
		Unit:    "s",
		GridPos: dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 9},
		Queries: []prom.QuerySpec{
			{
				Expr:   `histogram_quantile(0.99, sum by (le, controller) (rate(controller_runtime_reconcile_time_seconds_bucket` + controllerFilter + `[5m])))`,
				Legend: "{{controller}} p99",
			},
			{
				Expr:   `histogram_quantile(0.50, sum by (le, controller) (rate(controller_runtime_reconcile_time_seconds_bucket` + controllerFilter + `[5m])))`,
				Legend: "{{controller}} p50",
			},
		},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:      13,
		Title:   "Workqueue depth",
		GridPos: dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 9},
		Queries: []prom.QuerySpec{{
			Expr:   `workqueue_depth{name=~"ruleset|engine|rulesource"}`,
			Legend: "{{name}}",
		}},
	}))
}

func addValidation(builder *dashboard.DashboardBuilder) {
	const y uint32 = 24
	builder.WithRow(dashboard.NewRowBuilder("Validation").
		Id(14).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: y}),
	)

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          15,
		Title:       "RuleSource validation rate",
		Description: "RuleSource validations per second by outcome (valid=Coraza parse succeeded, invalid=parse failed, skipped=annotation or patch-only).",
		Unit:        "ops",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 1},
		Queries: []prom.QuerySpec{{
			Expr:   "sum by (outcome) (rate(coraza_rulesource_validations_total[5m]))",
			Legend: "{{outcome}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          16,
		Title:       "RuleSet validation rate",
		Description: "RuleSet aggregate validations per second (valid=Coraza parse succeeded; does not imply Ready).",
		Unit:        "ops",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 1},
		Queries: []prom.QuerySpec{{
			Expr:   "sum by (outcome) (rate(coraza_ruleset_validations_total[5m]))",
			Legend: "{{outcome}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          17,
		Title:       "RuleSource validation duration",
		Description: "RuleSource validation latency by outcome.",
		Unit:        "s",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 9},
		Queries: []prom.QuerySpec{
			{
				Expr:   "histogram_quantile(0.99, sum by (le, outcome) (rate(coraza_rulesource_validation_duration_seconds_bucket[5m])))",
				Legend: "{{outcome}} p99",
			},
			{
				Expr:   "histogram_quantile(0.50, sum by (le, outcome) (rate(coraza_rulesource_validation_duration_seconds_bucket[5m])))",
				Legend: "{{outcome}} p50",
			},
		},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          18,
		Title:       "RuleSet validation duration",
		Description: "RuleSet aggregate validation latency by outcome.",
		Unit:        "s",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 9},
		Queries: []prom.QuerySpec{
			{
				Expr:   "histogram_quantile(0.99, sum by (le, outcome) (rate(coraza_ruleset_validation_duration_seconds_bucket[5m])))",
				Legend: "{{outcome}} p99",
			},
			{
				Expr:   "histogram_quantile(0.50, sum by (le, outcome) (rate(coraza_ruleset_validation_duration_seconds_bucket[5m])))",
				Legend: "{{outcome}} p50",
			},
		},
	}))
}

func addCacheRED(builder *dashboard.DashboardBuilder) {
	const y uint32 = 40
	builder.WithRow(dashboard.NewRowBuilder("Cache server — RED").
		Id(19).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: y}),
	)

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:      20,
		Title:   "Request rate",
		Unit:    "reqps",
		GridPos: dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 1},
		Queries: []prom.QuerySpec{{
			Expr:   "sum by (handler, code) (rate(coraza_cache_server_requests_total[5m]))",
			Legend: "{{handler}} {{code}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          21,
		Title:       "Errors & auth failures",
		Description: "Cache server 5xx error rate and authentication failure rate.",
		Unit:        "reqps",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 1},
		Queries: []prom.QuerySpec{
			{
				Expr:   `sum(rate(coraza_cache_server_requests_total{code=~"5.."}[5m]))`,
				Legend: "5xx",
			},
			{
				Expr:   "rate(coraza_cache_server_auth_failures_total[5m])",
				Legend: "auth failures",
			},
		},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:      22,
		Title:   "Request latency",
		Unit:    "s",
		GridPos: dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 9},
		Queries: []prom.QuerySpec{
			{
				Expr:   "histogram_quantile(0.99, sum by (le, handler) (rate(coraza_cache_server_request_duration_seconds_bucket[5m])))",
				Legend: "{{handler}} p99",
			},
			{
				Expr:   "histogram_quantile(0.50, sum by (le, handler) (rate(coraza_cache_server_request_duration_seconds_bucket[5m])))",
				Legend: "{{handler}} p50",
			},
		},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:      23,
		Title:   "In-flight requests",
		GridPos: dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 9},
		Queries: []prom.QuerySpec{{
			Expr:   "coraza_cache_server_in_flight_requests",
			Legend: "{{handler}}",
		}},
	}))
}

func addCacheUSE(builder *dashboard.DashboardBuilder) {
	const y uint32 = 56
	builder.WithRow(dashboard.NewRowBuilder("Cache server — USE").
		Id(24).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: y}),
	)

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          25,
		Title:       "Cache size vs limit",
		Description: "RuleSet cache payload size vs configured maximum (always emitted by the operator).",
		Unit:        "bytes",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 1},
		Queries: []prom.QuerySpec{
			{Expr: "coraza_cache_size_bytes", Legend: "size"},
			{Expr: "coraza_cache_config_max_size_bytes", Legend: "max"},
		},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          26,
		Title:       "GC prune rate",
		Description: "Entries pruned per second by reason (age/size). Flat at zero until GC runs.",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 1},
		Queries: []prom.QuerySpec{{
			Expr:   "sum by (reason) (rate(coraza_cache_gc_pruned_entries_total[15m]))",
			Legend: "{{reason}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          27,
		Title:       "Cache instances & entries",
		Description: "Distinct cache keys and total stored revisions.",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 9},
		Queries: []prom.QuerySpec{
			{Expr: "coraza_cache_instances", Legend: "instances"},
			{Expr: "coraza_cache_total_entries", Legend: "entries"},
		},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          28,
		Title:       "Cache Put duration",
		Description: "Latency of RuleSet cache Put operations after successful validation.",
		Unit:        "s",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 9},
		Queries: []prom.QuerySpec{
			{
				Expr:   "histogram_quantile(0.99, sum by (le, namespace) (rate(coraza_cache_set_duration_seconds_bucket[5m])))",
				Legend: "{{namespace}} p99",
			},
			{
				Expr:   "histogram_quantile(0.50, sum by (le, namespace) (rate(coraza_cache_set_duration_seconds_bucket[5m])))",
				Legend: "{{namespace}} p50",
			},
		},
	}))
}

func addKubernetesAPI(builder *dashboard.DashboardBuilder) {
	const y uint32 = 72
	builder.WithRow(dashboard.NewRowBuilder("Kubernetes API & workqueue").
		Id(29).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: y}),
	)

	nameFilter := `{name=~"ruleset|engine|rulesource"}`

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          30,
		Title:       "API request rate",
		Description: "Kubernetes API request rate by verb and resource (from client-go).",
		Unit:        "reqps",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 1},
		Queries: []prom.QuerySpec{{
			Expr:   "sum by (verb, resource) (rate(rest_client_requests_total[5m]))",
			Legend: "{{verb}} {{resource}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          31,
		Title:       "API errors (non-2xx)",
		Description: "Non-success API responses by status code.",
		Unit:        "reqps",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 1},
		Queries: []prom.QuerySpec{{
			Expr:   `sum by (code) (rate(rest_client_requests_total{code!~"2.."}[5m]))`,
			Legend: "{{code}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          32,
		Title:       "API request latency p99",
		Description: "Kubernetes API call latency by verb.",
		Unit:        "s",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 9},
		Queries: []prom.QuerySpec{
			{
				Expr:   "histogram_quantile(0.99, sum by (le, verb) (rate(rest_client_request_duration_seconds_bucket[5m])))",
				Legend: "{{verb}} p99",
			},
			{
				Expr:   "histogram_quantile(0.50, sum by (le, verb) (rate(rest_client_request_duration_seconds_bucket[5m])))",
				Legend: "{{verb}} p50",
			},
		},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          33,
		Title:       "Workqueue retries",
		Description: "Workqueue retry rate per controller.",
		Unit:        "ops",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 9},
		Queries: []prom.QuerySpec{{
			Expr:   "sum by (name) (rate(workqueue_retries_total" + nameFilter + "[5m]))",
			Legend: "{{name}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          34,
		Title:       "Longest running processor",
		Description: "Duration of the longest currently-running reconcile per controller.",
		Unit:        "s",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: y + 17},
		Queries: []prom.QuerySpec{{
			Expr:   "workqueue_longest_running_processor_seconds" + nameFilter,
			Legend: "{{name}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          35,
		Title:       "Unfinished work",
		Description: "Total seconds of unfinished work in the queue per controller.",
		Unit:        "s",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: y + 17},
		Queries: []prom.QuerySpec{{
			Expr:   "workqueue_unfinished_work_seconds" + nameFilter,
			Legend: "{{name}}",
		}},
	}))
}

func addResourceCounts(builder *dashboard.DashboardBuilder) {
	const y uint32 = 96
	builder.WithRow(dashboard.NewRowBuilder("Resource counts by namespace").
		Id(36).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: y}),
	)

	resourceCounts := []struct {
		title  string
		metric string
		desc   string
	}{
		{"Engines", "coraza_engines", "Current per-namespace engine count (instant; empty when zero)."},
		{"RuleSets", "coraza_rulesets", "Current per-namespace ruleset count (instant; empty when zero)."},
		{"RuleSources", "coraza_rulesources", "Current per-namespace rulesource count (instant; empty when zero)."},
		{"RuleData", "coraza_ruledatas", "Current per-namespace ruledata count (instant; empty when zero)."},
	}

	for i, rc := range resourceCounts {
		builder.WithPanel(panels.ResourceCountTable(panels.ResourceCountTableConfig{
			ID:          uint32(37 + i),
			Title:       rc.title,
			Description: rc.desc,
			Expr:        rc.metric,
			GridPos: dashboard.GridPos{
				H: 6,
				W: 12,
				X: uint32((i % 2) * 12),
				Y: y + 1 + uint32(i/2)*6,
			},
		}))
	}
}

func floatPtr(v float64) *float64 {
	return &v
}
