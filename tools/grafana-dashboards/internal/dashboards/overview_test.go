package dashboards

import "testing"

func TestBuildOverview(t *testing.T) {
	dash, err := BuildOverview()
	if err != nil {
		t.Fatalf("BuildOverview: %v", err)
	}
	if dash.Uid == nil || *dash.Uid != "coraza-operator-overview" {
		t.Fatalf("unexpected uid: %v", dash.Uid)
	}
	if len(dash.Panels) != 40 {
		t.Fatalf("expected 40 panels (full overview), got %d", len(dash.Panels))
	}
}
