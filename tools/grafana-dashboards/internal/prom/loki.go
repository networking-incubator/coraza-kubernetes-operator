package prom

import (
	cog "github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/cog/variants"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/loki"
)

// LokiQuerySpec describes one Loki query target on a panel.
type LokiQuerySpec struct {
	Expr   string
	Legend string
}

// LokiDatasource returns the Loki datasource provisioned by the observability demo (uid: loki).
func LokiDatasource() common.DataSourceRef {
	return common.DataSourceRef{
		Type: cog.ToPtr("loki"),
		Uid:  cog.ToPtr("loki"),
	}
}

func LokiQueryWithRef(expr, refID, legend string) *loki.DataqueryBuilder {
	b := loki.NewDataqueryBuilder().
		Datasource(LokiDatasource()).
		Expr(expr).
		RefId(refID)
	if legend != "" {
		b = b.LegendFormat(legend)
	}
	return b
}

// LokiQueries builds Loki targets with refIds A, B, C, …
func LokiQueries(specs []LokiQuerySpec) []cog.Builder[variants.Dataquery] {
	targets := make([]cog.Builder[variants.Dataquery], len(specs))
	for i, spec := range specs {
		refID := string(rune('A' + i))
		targets[i] = LokiQueryWithRef(spec.Expr, refID, spec.Legend)
	}
	return targets
}
