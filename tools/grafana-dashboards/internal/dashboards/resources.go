package dashboards

import (
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/text"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/panels"
	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/prom"
)

// BuildResources generates the full Resources drill-down dashboard.
func BuildResources() (dashboard.Dashboard, error) {
	builder := resourcesBaseBuilder()
	addResourcesIntro(builder)
	addSelectedEngine(builder)
	addRuleSetsInNamespace(builder)
	addNamespaceInventory(builder)
	addOperatorActivity(builder)
	return builder.Build()
}

func addResourcesIntro(builder *dashboard.DashboardBuilder) {
	builder.WithPanel(text.NewPanelBuilder().
		Id(1).
		Title("").
		GridPos(dashboard.GridPos{H: 3, W: 24, X: 0, Y: 0}).
		Mode(text.TextModeMarkdown).
		Content(
			"Drill-down for Coraza CRs. Pick **Namespace** to scope all tables below. " +
				"Each table row is one resource (`namespace/name`). " +
				"**Engine** / **RuleSet** dropdowns optionally narrow the **Engine details** panel only. " +
				"Condition and composition tables always list every resource in the namespace.",
		),
	)
}

func addSelectedEngine(builder *dashboard.DashboardBuilder) {
	const rowY uint32 = 3

	builder.WithRow(dashboard.NewRowBuilder("Selected Engine").
		Id(2).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rowY}),
	)

	builder.WithPanel(panels.ResourceConditionTable(panels.ConditionTableConfig{
		ID:             3,
		Title:          "Engine conditions",
		Description:    "One row per Engine in the selected namespace(s).",
		Metric:         "coraza_engine_condition",
		Conditions:     []string{"Ready", "Progressing", "Degraded", "Accepted"},
		GridPos:        dashboard.GridPos{H: 6, W: 24, X: 0, Y: rowY + 1},
		SortByResource: true,
	}))

	builder.WithPanel(panels.TablePanel(panels.TablePanelConfig{
		ID:          4,
		Title:       "Engine details",
		Description: "Engine metadata (filtered by Engine dropdown; set All to list every engine).",
		GridPos:     dashboard.GridPos{H: 6, W: 24, X: 0, Y: rowY + 7},
		Expr:        `coraza_engine_info{namespace=~"$namespace", name=~"$engine"}`,
		Transformations: []dashboard.DataTransformerConfig{
			panels.OrganizeExclude("Time", "Value"),
		},
	}))
}

func addRuleSetsInNamespace(builder *dashboard.DashboardBuilder) {
	const rowY uint32 = 16

	builder.WithRow(dashboard.NewRowBuilder("RuleSets in namespace").
		Id(5).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rowY}),
	)

	builder.WithPanel(panels.ResourceConditionTable(panels.ConditionTableConfig{
		ID:             6,
		Title:          "RuleSet conditions",
		Description:    "One row per RuleSet in the selected namespace(s).",
		Metric:         "coraza_ruleset_condition",
		Conditions:     []string{"Ready", "Progressing", "Degraded"},
		GridPos:        dashboard.GridPos{H: 6, W: 24, X: 0, Y: rowY + 1},
		SortByResource: true,
	}))

	builder.WithPanel(panels.RulesetCompositionTable(panels.RulesetCompositionTableConfig{
		ID:          7,
		Title:       "RuleSet composition",
		Description: "RuleSource and RuleData reference counts per RuleSet.",
		GridPos:     dashboard.GridPos{H: 5, W: 24, X: 0, Y: rowY + 7},
	}))
}

func addNamespaceInventory(builder *dashboard.DashboardBuilder) {
	const enginesRowY uint32 = 28

	builder.WithRow(dashboard.NewRowBuilder("Namespace inventory — Engines").
		Id(8).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: enginesRowY}),
	)

	builder.WithPanel(panels.ResourceConditionTable(panels.ConditionTableConfig{
		ID:             9,
		Title:          "Engines in namespace",
		Description:    "Ready and Degraded condition values per engine (namespace/name).",
		Metric:         "coraza_engine_condition",
		Conditions:     []string{"Ready", "Degraded"},
		GridPos:        dashboard.GridPos{H: 10, W: 24, X: 0, Y: enginesRowY + 1},
		SortByResource: true,
	}))

	const rulesourcesRowY uint32 = 39
	builder.WithRow(dashboard.NewRowBuilder("Namespace inventory — RuleSources").
		Id(10).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rulesourcesRowY}),
	)

	builder.WithPanel(panels.ResourceConditionTable(panels.ConditionTableConfig{
		ID:             11,
		Title:          "RuleSources",
		Metric:         "coraza_rulesource_condition",
		Conditions:     []string{"Ready", "Degraded"},
		GridPos:        dashboard.GridPos{H: 8, W: 24, X: 0, Y: rulesourcesRowY + 1},
		SortByResource: true,
	}))
}

func addOperatorActivity(builder *dashboard.DashboardBuilder) {
	const rowY uint32 = 48
	const panelY uint32 = 49
	controllerFilter := `{controller=~"ruleset|engine|rulesource"}`

	builder.WithRow(dashboard.NewRowBuilder("Operator-wide activity").
		Id(12).
		GridPos(dashboard.GridPos{H: 1, W: 24, X: 0, Y: rowY}),
	)

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          13,
		Title:       "Reconcile rate (all controllers)",
		Description: "Operator-wide; not filtered by namespace variable.",
		Unit:        "ops",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 0, Y: panelY},
		Queries: []prom.QuerySpec{{
			Expr:   `sum by (controller, result) (rate(controller_runtime_reconcile_total` + controllerFilter + `[5m]))`,
			Legend: "{{controller}} {{result}}",
		}},
	}))

	builder.WithPanel(panels.TimeseriesPanel(panels.TimeseriesPanelConfig{
		ID:          14,
		Title:       "Cache request rate",
		Description: "Cache is operator-scoped; keys are ruleset namespace/name.",
		Unit:        "reqps",
		GridPos:     dashboard.GridPos{H: 8, W: 12, X: 12, Y: panelY},
		Queries: []prom.QuerySpec{{
			Expr:   "sum by (handler) (rate(coraza_cache_server_requests_total[5m]))",
			Legend: "{{handler}}",
		}},
	}))
}
