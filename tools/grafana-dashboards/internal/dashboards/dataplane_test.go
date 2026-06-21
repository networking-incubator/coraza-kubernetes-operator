package dashboards

import "testing"

func TestBuildDataplane(t *testing.T) {
	dash, err := BuildDataplane()
	if err != nil {
		t.Fatalf("BuildDataplane: %v", err)
	}
	if dash.Uid == nil || *dash.Uid != "coraza-waf-dataplane" {
		t.Fatalf("unexpected uid: %v", dash.Uid)
	}
	if len(dash.Panels) != 20 {
		t.Fatalf("expected 20 panels (full dataplane dashboard), got %d", len(dash.Panels))
	}
}
