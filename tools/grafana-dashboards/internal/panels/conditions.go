package panels

import (
	"fmt"
	"strings"

	cog "github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/table"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/prom"
)

// ConditionTableConfig configures a pivoted resource condition table.
type ConditionTableConfig struct {
	ID             uint32
	Title          string
	Description    string
	Metric         string
	Conditions     []string
	GridPos        dashboard.GridPos
	MatchNameVar   string // optional templating variable name (without $)
	SortByResource bool
}

// RulesetCompositionTableConfig configures the RuleSet sources/data merge table.
type RulesetCompositionTableConfig struct {
	ID           uint32
	Title        string
	Description  string
	GridPos      dashboard.GridPos
	MatchNameVar string
}

// ResourceConditionTable builds a condition pivot table (groupingToMatrix → renameByRegex → organize).
func ResourceConditionTable(cfg ConditionTableConfig) *table.PanelBuilder {
	condRE := strings.Join(cfg.Conditions, "|")
	labelSelector := `namespace=~"$namespace"`
	if cfg.MatchNameVar != "" {
		labelSelector += fmt.Sprintf(`, name=~"$%s"`, cfg.MatchNameVar)
	}
	expr := fmt.Sprintf(
		`label_join(%s{%s, condition=~"%s"}, "resource", "/", "namespace", "name")`,
		cfg.Metric, labelSelector, condRE,
	)

	b := table.NewPanelBuilder().
		Id(cfg.ID).
		Title(cfg.Title).
		Description(cfg.Description).
		Datasource(prom.Datasource()).
		GridPos(cfg.GridPos).
		CellHeight(common.TableCellHeightSm).
		ShowHeader(true).
		Footer(common.NewTableFooterOptionsBuilder().Show(false)).
		ColorScheme(dashboard.NewFieldColorBuilder().Mode(dashboard.FieldColorModeIdThresholds)).
		WithTarget(prom.TableQuery(expr, "A"))

	for _, cond := range cfg.Conditions {
		b = b.OverrideByName(cond, conditionFieldOverrides(cond))
	}

	for _, t := range conditionTableTransformations(cfg.Conditions) {
		b = b.WithTransformation(t)
	}

	if cfg.SortByResource {
		b = b.SortBy([]cog.Builder[common.TableSortByFieldState]{
			common.NewTableSortByFieldStateBuilder().DisplayName("Resource").Desc(false),
		})
	}

	return b
}

// RulesetCompositionTable builds the merged RuleSet sources/data files table.
func RulesetCompositionTable(cfg RulesetCompositionTableConfig) *table.PanelBuilder {
	labelSelector := `namespace=~"$namespace"`
	if cfg.MatchNameVar != "" {
		labelSelector += fmt.Sprintf(`, name=~"$%s"`, cfg.MatchNameVar)
	}

	b := table.NewPanelBuilder().
		Id(cfg.ID).
		Title(cfg.Title).
		Description(cfg.Description).
		Datasource(prom.Datasource()).
		GridPos(cfg.GridPos).
		CellHeight(common.TableCellHeightSm).
		ShowHeader(true).
		Footer(common.NewTableFooterOptionsBuilder().Show(false)).
		WithTarget(prom.TableQuery(fmt.Sprintf("coraza_ruleset_sources{%s}", labelSelector), "A")).
		WithTarget(prom.TableQuery(fmt.Sprintf("coraza_ruleset_data_files{%s}", labelSelector), "B")).
		SortBy([]cog.Builder[common.TableSortByFieldState]{
			common.NewTableSortByFieldStateBuilder().DisplayName("namespace").Desc(false),
		})

	b = b.WithTransformation(dashboard.DataTransformerConfig{Id: "merge", Options: map[string]any{}})
	b = b.WithTransformation(dashboard.DataTransformerConfig{
		Id: "organize",
		Options: map[string]any{
			"excludeByName": map[string]bool{"Time": true},
			"indexByName": map[string]int{
				"namespace": 0,
				"name":      1,
				"Value #A":  2,
				"Value #B":  3,
			},
			"renameByName": map[string]string{
				"Value #A": "Sources",
				"Value #B": "Data files",
			},
		},
	})

	return b
}

func conditionTableTransformations(conditions []string) []dashboard.DataTransformerConfig {
	indexByName := map[string]int{"Resource": 0}
	for i, cond := range conditions {
		indexByName[cond] = i + 1
	}

	return []dashboard.DataTransformerConfig{
		{
			Id: "groupingToMatrix",
			Options: map[string]any{
				"columnField": "condition",
				"rowField":    "resource",
				"valueField":  "Value",
				"emptyValue":  "null",
			},
		},
		{
			Id: "renameByRegex",
			Options: map[string]any{
				"regex":         `resource\\condition`,
				"renamePattern": "Resource",
			},
		},
		{
			Id: "organize",
			Options: map[string]any{
				"excludeByName": map[string]bool{"Time": true},
				"indexByName":   indexByName,
			},
		},
	}
}

func conditionFieldOverrides(condition string) []dashboard.DynamicConfigValue {
	return []dashboard.DynamicConfigValue{
		{Id: "mappings", Value: conditionValueMapping(condition)},
		{Id: "custom.cellOptions", Value: map[string]string{"type": "color-background"}},
		{Id: "thresholds", Value: absoluteThresholdsJSON(conditionStatSteps(condition))},
	}
}

func conditionValueMapping(condition string) dashboard.ValueMap {
	absentText := "Absent"
	if condition == "Progressing" || condition == "Degraded" {
		absentText = "No"
	}
	idx0, idx1, idx2 := int32(0), int32(1), int32(2)
	return dashboard.ValueMap{
		Type: dashboard.MappingTypeValueToText,
		Options: map[string]dashboard.ValueMappingResult{
			"-1": {Text: cog.ToPtr(absentText), Index: &idx0},
			"0":  {Text: cog.ToPtr("False"), Index: &idx1},
			"1":  {Text: cog.ToPtr("True"), Index: &idx2},
		},
	}
}

func conditionStatSteps(condition string) []ThresholdStep {
	switch condition {
	case "Progressing":
		return []ThresholdStep{{Color: "green"}, {Value: floatPtr(1), Color: "yellow"}}
	case "Degraded":
		return []ThresholdStep{{Color: "green"}, {Value: floatPtr(1), Color: "red"}}
	default:
		return []ThresholdStep{
			{Color: "red"},
			{Value: floatPtr(0.5), Color: "yellow"},
			{Value: floatPtr(1), Color: "green"},
		}
	}
}

// absoluteThresholdsJSON returns a map matching Grafana JSON threshold shape for DynamicConfigValue.
func absoluteThresholdsJSON(steps []ThresholdStep) map[string]any {
	thresholdSteps := make([]map[string]any, len(steps))
	for i, step := range steps {
		thresholdSteps[i] = map[string]any{
			"color": step.Color,
			"value": step.Value,
		}
	}
	return map[string]any{
		"mode":  "absolute",
		"steps": thresholdSteps,
	}
}

func floatPtr(v float64) *float64 {
	return &v
}
