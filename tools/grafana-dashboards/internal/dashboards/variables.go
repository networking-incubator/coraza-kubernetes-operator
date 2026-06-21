package dashboards

import (
	cog "github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
)

const namespaceLabelValuesQuery = `label_values({__name__=~"coraza_(engines|rulesets|rulesources|ruledatas|` +
	`engine_info|ruleset_info|rulesource_info|ruledata_info)"}, namespace)`

func allVariableCurrent() dashboard.VariableOption {
	return dashboard.VariableOption{
		Selected: cog.ToPtr(true),
		Text:     dashboard.StringOrArrayOfString{String: cog.ToPtr("All")},
		Value:    dashboard.StringOrArrayOfString{String: cog.ToPtr("$__all")},
	}
}

func prometheusQueryVariable(name, label, definition string) *dashboard.QueryVariableBuilder {
	return dashboard.NewQueryVariableBuilder(name).
		Label(label).
		Datasource(promDatasourceRef()).
		Definition(definition).
		Query(prometheusVariableQuery(definition)).
		Current(allVariableCurrent()).
		Multi(true).
		IncludeAll(true).
		AllValue(".*").
		Refresh(dashboard.VariableRefreshOnTimeRangeChanged).
		Sort(dashboard.VariableSortAlphabeticalAsc)
}

func prometheusVariableQuery(query string) dashboard.StringOrMap {
	return dashboard.StringOrMap{
		Map: map[string]any{
			"query": query,
			"refId": "PrometheusVariableQueryEditor-VariableQuery",
		},
	}
}

func promDatasourceRef() common.DataSourceRef {
	return common.DataSourceRef{
		Type: cog.ToPtr("prometheus"),
		Uid:  cog.ToPtr("${DS_PROMETHEUS}"),
	}
}

func lokiDatasourceRef() common.DataSourceRef {
	return common.DataSourceRef{
		Type: cog.ToPtr("loki"),
		Uid:  cog.ToPtr("loki"),
	}
}

const (
	dataplaneNamespaceQuery = `label_values(coraza_waf_requests_total, namespace)`
	dataplaneEngineQuery    = `label_values(coraza_waf_requests_total{namespace=~"$namespace"}, engine)`
)

func dataplaneCustomVariable(name, label, defaultValue string) *dashboard.CustomVariableBuilder {
	current := dashboard.VariableOption{
		Selected: cog.ToPtr(true),
		Text:     dashboard.StringOrArrayOfString{String: cog.ToPtr(defaultValue)},
		Value:    dashboard.StringOrArrayOfString{String: cog.ToPtr(defaultValue)},
	}
	return dashboard.NewCustomVariableBuilder(name).
		Label(label).
		Values(dashboard.StringOrMap{String: cog.ToPtr(defaultValue)}).
		Current(current).
		Options([]dashboard.VariableOption{current}).
		IncludeAll(true).
		Multi(true).
		AllValue(".+").
		AllowCustomValue(true)
}

func prometheusDatasourceVariable() *dashboard.DatasourceVariableBuilder {
	return dashboard.NewDatasourceVariableBuilder("DS_PROMETHEUS").
		Label("Datasource").
		Type("prometheus").
		Current(dashboard.VariableOption{
			Selected: cog.ToPtr(true),
			Text:     dashboard.StringOrArrayOfString{String: cog.ToPtr("Prometheus")},
			Value:    dashboard.StringOrArrayOfString{String: cog.ToPtr("prometheus")},
		})
}

func builtInAnnotations() *dashboard.AnnotationQueryBuilder {
	return dashboard.NewAnnotationQueryBuilder().
		Name("Annotations & Alerts").
		Enable(true).
		Hide(true).
		IconColor("rgba(0, 211, 255, 1)").
		Type("dashboard").
		BuiltIn(1).
		Datasource(common.DataSourceRef{
			Type: cog.ToPtr("grafana"),
			Uid:  cog.ToPtr("-- Grafana --"),
		})
}

func resourcesBaseBuilder() *dashboard.DashboardBuilder {
	return dashboard.NewDashboardBuilder("Coraza Operator — Resources").
		Uid("coraza-operator-resources").
		Tags([]string{"coraza", "operator", "control-plane", "waf"}).
		Timezone("browser").
		Editable().
		Tooltip(dashboard.DashboardCursorSyncCrosshair).
		Refresh("30s").
		Time("now-1h", "now").
		Version(1).
		Link(dashboard.NewDashboardLinkBuilder("Overview").
			Type(dashboard.DashboardLinkTypeLink).
			Url("/d/coraza-operator-overview/coraza-operator-overview?${__url_time_range}").
			Icon("dashboard"),
		).
		WithVariable(dashboard.NewDatasourceVariableBuilder("DS_PROMETHEUS").
			Label("Datasource").
			Type("prometheus"),
		).
		WithVariable(prometheusQueryVariable("namespace", "Namespace", namespaceLabelValuesQuery)).
		WithVariable(prometheusQueryVariable(
			"engine",
			"Engine",
			`label_values(coraza_engine_info{namespace=~"$namespace"}, name)`,
		)).
		WithVariable(prometheusQueryVariable(
			"ruleset",
			"RuleSet",
			`label_values(coraza_ruleset_info{namespace=~"$namespace"}, name)`,
		)).
		Annotation(builtInAnnotations())
}
