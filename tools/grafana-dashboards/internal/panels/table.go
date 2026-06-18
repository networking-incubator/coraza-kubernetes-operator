package panels

import (
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/table"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/prom"
)

// TablePanelConfig configures a table panel.
type TablePanelConfig struct {
	ID              uint32
	Title           string
	Description     string
	GridPos         dashboard.GridPos
	Expr            string
	Transformations []dashboard.DataTransformerConfig
}

// TablePanel builds a table panel with optional transformations.
func TablePanel(cfg TablePanelConfig) *table.PanelBuilder {
	b := table.NewPanelBuilder().
		Id(cfg.ID).
		Title(cfg.Title).
		Description(cfg.Description).
		Datasource(prom.Datasource()).
		GridPos(cfg.GridPos).
		CellHeight(common.TableCellHeightSm).
		ShowHeader(true).
		Footer(common.NewTableFooterOptionsBuilder().Show(false)).
		WithTarget(prom.TableQuery(cfg.Expr, "A"))

	for _, t := range cfg.Transformations {
		b = b.WithTransformation(t)
	}
	return b
}

// OrganizeExclude hides named columns via an organize transformation.
func OrganizeExclude(columns ...string) dashboard.DataTransformerConfig {
	exclude := make(map[string]bool, len(columns))
	for _, col := range columns {
		exclude[col] = true
	}
	return dashboard.DataTransformerConfig{
		Id: "organize",
		Options: map[string]any{
			"excludeByName": exclude,
		},
	}
}
