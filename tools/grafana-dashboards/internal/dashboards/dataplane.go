package dashboards

import (
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/text"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/panels"
	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/prom"
)

// BuildDataplane generates the WAF dataplane (Envoy / coraza_waf_*) dashboard.
func BuildDataplane() (dashboard.Dashboard, error) {
	builder := dataplaneBaseBuilder()
	addDataplaneIntro(builder)
	addDataplaneSummary(builder)
	addDataplaneTraffic(builder)
	addDataplaneRules(builder)
	addDataplaneAnomaly(builder)
	addDataplanePlugin(builder)
	addDataplaneLogs(builder)
	return builder.Build()
}

func dataplaneBaseBuilder() *dashboard.DashboardBuilder {
	return dashboard.NewDashboardBuilder("Coraza WAF — Dataplane").
		Uid("coraza-waf-dataplane").
		Tags([]string{"coraza", "operator", "dataplane", "waf"}).
		Timezone("browser").
		Editable().
		Tooltip(dashboard.DashboardCursorSyncCrosshair).
		Refresh("30s").
		Time("now-1h", "now").
		Version(2).
		Link(dashboard.NewDashboardLinkBuilder("Control-plane overview").
			Type(dashboard.DashboardLinkTypeLink).
			Url("/d/coraza-operator-overview/coraza-operator-overview?${__url_time_range}").
			Icon("dashboard"),
		).
		WithVariable(prometheusDatasourceVariable()).
		WithVariable(prometheusQueryVariable("namespace", "Namespace", dataplaneNamespaceQuery)).
		WithVariable(prometheusQueryVariable("engine", "Engine", dataplaneEngineQuery)).
		Annotation(builtInAnnotations())
}

func addDataplaneIntro(builder *dashboard.DashboardBuilder) {
	builder.WithPanel(text.NewPanelBuilder().
		Id(1).
		Title("").
		GridPos(dashboard.GridPos{H: 3, W: 24, X: 0, Y: 0}).
		Mode(text.TextModeMarkdown).
		Content(
			"Live **coraza_waf_*** metrics from Envoy Gateway pods (Proxy-WASM contract mode). " +
				"Select the Engine namespace and name from the dropdowns (e.g. `integration-tests` / `coraza` for the demo, " +
				"`crs-conformance-*` / `conformance-engine` during FTW).",
		),
	)
}

func addDataplaneSummary(builder *dashboard.DashboardBuilder) {
	const rowY uint32 = 3
	builder.WithRow(dashboard.NewRowBuilder("Traffic summary").
		Id(2).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rowY}),
	)

	filter := dataplaneLabels()
	stats := []panels.StatPanelConfig{
		{
			ID:          3,
			Title:       "Request rate",
			Description: "All WAF-inspected requests per second.",
			Expr:        `sum(rate(coraza_waf_requests_total` + filter + `[5m]))`,
			Unit:        "reqps",
			GridPos:     dashboard.GridPos{H: 4, W: 6, X: 0, Y: rowY + 1},
		},
		{
			ID:          4,
			Title:       "Block rate",
			Description: "Blocked requests per second (outcome=block).",
			Expr:        `sum(rate(coraza_waf_requests_total` + dataplaneLabels(`outcome="block"`) + `[5m]))`,
			Unit:        "reqps",
			Steps: []panels.ThresholdStep{
				{Color: "green"},
				{Value: floatPtr(0.01), Color: "yellow"},
				{Value: floatPtr(1), Color: "red"},
			},
			GridPos: dashboard.GridPos{H: 4, W: 6, X: 6, Y: rowY + 1},
		},
		{
			ID:    5,
			Title: "Block ratio",
			Description: "Share of requests blocked in the selected window. " +
				"No data when there is no traffic.",
			Expr: `(sum(rate(coraza_waf_requests_total` + dataplaneLabels(`outcome="block"`) + `[5m])) / ` +
				`sum(rate(coraza_waf_requests_total` + filter + `[5m]))) ` +
				`and sum(rate(coraza_waf_requests_total` + filter + `[5m])) > 0`,
			Unit: "percentunit",
			Steps: []panels.ThresholdStep{
				{Color: "green"},
				{Value: floatPtr(0.05), Color: "yellow"},
				{Value: floatPtr(0.2), Color: "red"},
			},
			GridPos: dashboard.GridPos{H: 4, W: 6, X: 12, Y: rowY + 1},
		},
		{
			ID:          6,
			Title:       "Loaded rules",
			Description: "Rule count reported by the WASM plugin after last successful load.",
			Expr:        `max(coraza_waf_plugin_rule_count` + filter + `)`,
			Unit:        "none",
			GridPos:     dashboard.GridPos{H: 4, W: 6, X: 18, Y: rowY + 1},
		},
	}

	for _, stat := range stats {
		builder.WithPanel(panels.StatPanel(stat))
	}
}

func addDataplaneTraffic(builder *dashboard.DashboardBuilder) {
	const rowY uint32 = 8
	filter := dataplaneLabels()

	builder.WithRow(dashboard.NewRowBuilder("Request outcomes").
		Id(7).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rowY}),
	)

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          8,
		Title:       "Requests by outcome",
		Description: "pass, block, detect, redirect, error — per second.",
		Unit:        "reqps",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: rowY + 1},
		Queries: []prom.QuerySpec{{
			Expr:   `sum by (outcome) (rate(coraza_waf_requests_total` + filter + `[5m]))`,
			Legend: "{{outcome}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          9,
		Title:       "Blocked by category",
		Description: "Derived from CRS attack-* rule tags on blocking rules.",
		Unit:        "reqps",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: rowY + 1},
		Queries: []prom.QuerySpec{{
			Expr:   `sum by (category) (rate(coraza_waf_blocked_requests_total` + filter + `[5m]))`,
			Legend: "{{category}}",
		}},
	}))
}

func addDataplaneRules(builder *dashboard.DashboardBuilder) {
	const rowY uint32 = 17
	filter := dataplaneLabels()

	builder.WithRow(dashboard.NewRowBuilder("Rule activity").
		Id(10).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rowY}),
	)

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          11,
		Title:       "Rule hits (top rules)",
		Description: "Matched rules per second; overflow bucket uses rule_id=other.",
		Unit:        "ops",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: rowY + 1},
		Queries: []prom.QuerySpec{{
			Expr: `topk(10, sum by (rule_id) (` +
				`rate(coraza_waf_rule_hits_total` + filter + `[5m])))`,
			Legend: "{{rule_id}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          12,
		Title:       "Blocked by severity",
		Description: "Blocking events grouped by rule severity label.",
		Unit:        "reqps",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: rowY + 1},
		Queries: []prom.QuerySpec{{
			Expr:   `sum by (severity) (rate(coraza_waf_blocked_requests_total` + filter + `[5m]))`,
			Legend: "{{severity}}",
		}},
	}))
}

func addDataplaneAnomaly(builder *dashboard.DashboardBuilder) {
	const rowY uint32 = 26
	filter := dataplaneLabels()

	builder.WithRow(dashboard.NewRowBuilder("Anomaly scoring").
		Id(13).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rowY}),
	)

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          14,
		Title:       "Anomaly score quantiles",
		Description: "Transaction anomaly score distribution from matched rules.",
		Unit:        "none",
		GridPos:     dashboard.GridPos{H: 8, W: 24, X: 0, Y: rowY + 1},
		Queries: []prom.QuerySpec{
			{
				Expr: `histogram_quantile(0.99, sum by (le) (` +
					`rate(coraza_waf_request_anomaly_score_bucket` + filter + `[5m])))`,
				Legend: "p99",
			},
			{
				Expr: `histogram_quantile(0.50, sum by (le) (` +
					`rate(coraza_waf_request_anomaly_score_bucket` + filter + `[5m])))`,
				Legend: "p50",
			},
		},
	}))
}

func addDataplanePlugin(builder *dashboard.DashboardBuilder) {
	const rowY uint32 = 35
	filter := dataplaneLabels()

	builder.WithRow(dashboard.NewRowBuilder("Plugin lifecycle").
		Id(15).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rowY}),
	)

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          16,
		Title:       "Plugin loads",
		Description: "WASM plugin (re)load outcomes from the RuleSet cache poll path.",
		Unit:        "ops",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: rowY + 1},
		Queries: []prom.QuerySpec{{
			Expr:   `sum by (status) (rate(coraza_waf_plugin_loads_total` + filter + `[5m]))`,
			Legend: "{{status}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          17,
		Title:       "Rule overrides",
		Description: "Active SecRule override gauges (disabled rules, action changes, etc.).",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: rowY + 1},
		Queries: []prom.QuerySpec{{
			Expr:   `sum by (type) (coraza_waf_rule_overrides` + filter + `)`,
			Legend: "{{type}}",
		}},
	}))
}

func addDataplaneLogs(builder *dashboard.DashboardBuilder) {
	const rowY uint32 = 44
	logFilter := dataplaneLogLabels(`event="coraza_waf_blocked_request"`)

	builder.WithRow(dashboard.NewRowBuilder("WAF audit logs (Loki)").
		Id(18).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rowY}),
	)

	builder.WithPanel(panels.LokiTimeseriesPanel(panels.LokiTimeseriesPanelConfig{
		ID:          19,
		Title:       "Blocked request log events",
		Description: "WAF block events in Loki (contract JSON or CRS audit lines parsed by Promtail).",
		Unit:        "ops",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: rowY + 1},
		Queries: []prom.LokiQuerySpec{{
			Expr:   `sum(count_over_time(` + logFilter + ` [5m]))`,
			Legend: "blocks/5m window",
		}},
	}))

	builder.WithPanel(panels.LokiTimeseriesPanel(panels.LokiTimeseriesPanelConfig{
		ID:          20,
		Title:       "Blocked logs by category",
		Description: "Loki block events grouped by Promtail-extracted category label (CRS attack-* tags).",
		Unit:        "ops",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: rowY + 1},
		Queries: []prom.LokiQuerySpec{{
			Expr:   `sum by (category) (count_over_time(` + logFilter + ` [5m]))`,
			Legend: "{{category}}",
		}},
	}))
}
