package panels

import (
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/stat"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/prom"
)

// StatPanelConfig configures a single-value stat panel.
type StatPanelConfig struct {
	ID          uint32
	Title       string
	Description string
	Expr        string
	Unit        string
	GridPos     dashboard.GridPos
	Steps       []ThresholdStep
}

// StatPanel builds a stat panel matching the Python generator defaults.
func StatPanel(cfg StatPanelConfig) *stat.PanelBuilder {
	return stat.NewPanelBuilder().
		Id(cfg.ID).
		Title(cfg.Title).
		Description(cfg.Description).
		Datasource(prom.Datasource()).
		GridPos(cfg.GridPos).
		Unit(cfg.Unit).
		ColorScheme(dashboard.NewFieldColorBuilder().Mode(dashboard.FieldColorModeIdThresholds)).
		Thresholds(AbsoluteThresholds(cfg.Steps)).
		ColorMode(common.BigValueColorModeBackground).
		GraphMode(common.BigValueGraphModeNone).
		ReduceOptions(common.NewReduceDataOptionsBuilder().Calcs([]string{"lastNotNull"})).
		WithTarget(prom.Query(cfg.Expr))
}
