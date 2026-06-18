package dashboards

import (
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
)

func overviewBaseBuilder() *dashboard.DashboardBuilder {
	return dashboard.NewDashboardBuilder("Coraza Operator — Overview").
		Uid("coraza-operator-overview").
		Tags([]string{"coraza", "operator", "control-plane", "waf"}).
		Timezone("browser").
		Editable().
		Tooltip(dashboard.DashboardCursorSyncCrosshair).
		Refresh("30s").
		Time("now-1h", "now").
		Version(1).
		Link(dashboard.NewDashboardLinkBuilder("Resource drill-down").
			Type(dashboard.DashboardLinkTypeLink).
			Url("/d/coraza-operator-resources/coraza-operator-resources?${__url_time_range}").
			Icon("external link"),
		).
		WithVariable(dashboard.NewDatasourceVariableBuilder("DS_PROMETHEUS").
			Label("Datasource").
			Type("prometheus"),
		).
		Annotation(builtInAnnotations())
}
