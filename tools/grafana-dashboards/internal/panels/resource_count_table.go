package panels

import (
	cog "github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/table"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/prom"
)

// ResourceCountTableConfig configures a per-namespace resource count table.
type ResourceCountTableConfig struct {
	ID          uint32
	Title       string
	Description string
	Expr        string
	GridPos     dashboard.GridPos
}

// ResourceCountTable builds an instant Prometheus table with namespace and count
// columns. Unlike bar gauges, the namespace label stays visible with one row.
func ResourceCountTable(cfg ResourceCountTableConfig) *table.PanelBuilder {
	return table.NewPanelBuilder().
		Id(cfg.ID).
		Title(cfg.Title).
		Description(cfg.Description).
		Datasource(prom.Datasource()).
		GridPos(cfg.GridPos).
		CellHeight(common.TableCellHeightSm).
		ShowHeader(true).
		Footer(common.NewTableFooterOptionsBuilder().Show(false)).
		WithTarget(prom.TableQuery(cfg.Expr, "A")).
		WithTransformation(dashboard.DataTransformerConfig{
			Id: "organize",
			Options: map[string]any{
				"excludeByName": map[string]bool{
					"Time":     true,
					"__name__": true,
				},
				"indexByName": map[string]int{
					"namespace": 0,
					"Value":     1,
				},
				"renameByName": map[string]string{
					"Value": "Count",
				},
			},
		}).
		SortBy([]cog.Builder[common.TableSortByFieldState]{
			common.NewTableSortByFieldStateBuilder().DisplayName("namespace").Desc(false),
		})
}
