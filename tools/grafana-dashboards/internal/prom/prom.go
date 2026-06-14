package prom

import (
	cog "github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/cog/variants"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/prometheus"
)

// QuerySpec describes one Prometheus target on a panel.
type QuerySpec struct {
	Expr   string
	Legend string
}

// Datasource returns the Prometheus datasource variable reference used by all panels.
func Datasource() common.DataSourceRef {
	return common.DataSourceRef{
		Type: cog.ToPtr("prometheus"),
		Uid:  cog.ToPtr("${DS_PROMETHEUS}"),
	}
}

// Query builds a range Prometheus target with refId A.
func Query(expr string) *prometheus.DataqueryBuilder {
	return QueryWithRef(expr, "A", "")
}

// TableQuery builds an instant Prometheus target formatted as a table.
func TableQuery(expr, refID string) *prometheus.DataqueryBuilder {
	return prometheus.NewDataqueryBuilder().
		Datasource(Datasource()).
		Expr(expr).
		RefId(refID).
		Instant().
		Format(prometheus.PromQueryFormatTable)
}

// QueryWithRef builds a range Prometheus target with legend and refId.
func QueryWithRef(expr, refID, legend string) *prometheus.DataqueryBuilder {
	b := prometheus.NewDataqueryBuilder().
		Datasource(Datasource()).
		Expr(expr).
		RefId(refID).
		Range()
	if legend != "" {
		b = b.LegendFormat(legend)
	}
	return b
}

// InstantQueryWithRef builds an instant Prometheus target with legend and refId.
func InstantQueryWithRef(expr, refID, legend string) *prometheus.DataqueryBuilder {
	b := prometheus.NewDataqueryBuilder().
		Datasource(Datasource()).
		Expr(expr).
		RefId(refID).
		Instant()
	if legend != "" {
		b = b.LegendFormat(legend)
	}
	return b
}

// Queries builds range Prometheus targets with refIds A, B, C, …
func Queries(specs []QuerySpec) []cog.Builder[variants.Dataquery] {
	targets := make([]cog.Builder[variants.Dataquery], len(specs))
	for i, spec := range specs {
		refID := string(rune('A' + i))
		targets[i] = QueryWithRef(spec.Expr, refID, spec.Legend)
	}
	return targets
}
