package panels

import (
	cog "github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/cog/variants"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/timeseries"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/prom"
)

// LokiTimeseriesPanelConfig configures a timeseries panel backed by Loki queries.
type LokiTimeseriesPanelConfig struct {
	ID          uint32
	Title       string
	Description string
	Unit        string
	GridPos     dashboard.GridPos
	Queries     []prom.LokiQuerySpec
}

// LokiTimeseriesPanel builds a timeseries panel using the Loki datasource.
func LokiTimeseriesPanel(cfg LokiTimeseriesPanelConfig) *timeseries.PanelBuilder {
	unit := cfg.Unit
	if unit == "" {
		unit = "short"
	}

	spanNulls := common.BoolOrFloat64{Bool: cog.ToPtr(true)}

	return timeseries.NewPanelBuilder().
		Id(cfg.ID).
		Title(cfg.Title).
		Description(cfg.Description).
		Datasource(prom.LokiDatasource()).
		GridPos(cfg.GridPos).
		Unit(unit).
		ColorScheme(dashboard.NewFieldColorBuilder().Mode(dashboard.FieldColorModeIdPaletteClassic)).
		DrawStyle(common.GraphDrawStyleLine).
		FillOpacity(10).
		LineWidth(1).
		ShowPoints(common.VisibilityModeNever).
		AxisSoftMin(0).
		SpanNulls(spanNulls).
		Legend(common.NewVizLegendOptionsBuilder().
			DisplayMode(common.LegendDisplayModeList).
			Placement(common.LegendPlacementBottom).
			ShowLegend(true),
		).
		Tooltip(common.NewVizTooltipOptionsBuilder().
			Mode(common.TooltipDisplayModeMulti).
			Sort(common.SortOrderDescending),
		).
		Targets(lokiQueries(cfg.Queries))
}

func lokiQueries(specs []prom.LokiQuerySpec) []cog.Builder[variants.Dataquery] {
	return prom.LokiQueries(specs)
}
