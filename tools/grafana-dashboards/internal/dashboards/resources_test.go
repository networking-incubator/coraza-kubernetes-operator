package dashboards

import "testing"

func TestBuildResources(t *testing.T) {
	dash, err := BuildResources()
	if err != nil {
		t.Fatalf("BuildResources: %v", err)
	}
	if dash.Uid == nil || *dash.Uid != "coraza-operator-resources" {
		t.Fatalf("unexpected uid: %v", dash.Uid)
	}
	if len(dash.Templating.List) < 4 {
		t.Fatalf("expected DS + namespace + engine + ruleset variables, got %d", len(dash.Templating.List))
	}
	if len(dash.Panels) != 14 {
		t.Fatalf("expected 14 panels (full resources), got %d", len(dash.Panels))
	}
}
