package panels

import (
	"testing"

	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
)

func TestConditionTableTransformations(t *testing.T) {
	conditions := []string{"Ready", "Degraded"}
	transforms := conditionTableTransformations(conditions)
	if len(transforms) != 3 {
		t.Fatalf("expected 3 transformations, got %d", len(transforms))
	}
	if transforms[0].Id != "groupingToMatrix" {
		t.Fatalf("unexpected first transform: %s", transforms[0].Id)
	}
	organize := transforms[2].Options.(map[string]any)
	indexByName := organize["indexByName"].(map[string]int)
	if indexByName["Resource"] != 0 || indexByName["Ready"] != 1 || indexByName["Degraded"] != 2 {
		t.Fatalf("unexpected indexByName: %v", indexByName)
	}
}

func TestResourceConditionTableBuild(t *testing.T) {
	panel, err := ResourceConditionTable(ConditionTableConfig{
		ID:         1,
		Title:      "Test",
		Metric:     "coraza_engine_condition",
		Conditions: []string{"Ready"},
		GridPos:    dashboard.GridPos{H: 6, W: 24},
	}).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if panel.Type != "table" {
		t.Fatalf("expected table panel, got %s", panel.Type)
	}
	if len(panel.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(panel.Targets))
	}
}
