package dashboards

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/golden/*.semantic.json from the generator")

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}

func TestCommittedDashboardsMatchGenerator(t *testing.T) {
	cases := []struct {
		name  string
		file  string
		build func() (any, error)
	}{
		{"overview", "coraza-operator-overview.json", func() (any, error) { return BuildOverview() }},
		{"resources", "coraza-operator-resources.json", func() (any, error) { return BuildResources() }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			committedPath := filepath.Join(chartDashboardsDir(), tc.file)
			committedData, err := os.ReadFile(committedPath)
			if err != nil {
				t.Fatalf("read committed %s: %v", committedPath, err)
			}

			dash, err := tc.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			generated, err := SemanticsFromDashboard(dash)
			if err != nil {
				t.Fatalf("semantics from generator: %v", err)
			}
			committed, err := SemanticsFromJSON(committedData)
			if err != nil {
				t.Fatalf("semantics from committed: %v", err)
			}

			if !reflect.DeepEqual(committed, generated) {
				t.Fatalf("committed chart JSON is stale; run: make observability.dashboard.generate\ncommitted: %+v\ngenerated: %+v", committed, generated)
			}
		})
	}
}

func TestDashboardSemanticsGolden(t *testing.T) {
	cases := []struct {
		name    string
		golden  string
		build   func() (any, error)
	}{
		{"overview", "overview.semantic.json", func() (any, error) { return BuildOverview() }},
		{"resources", "resources.semantic.json", func() (any, error) { return BuildResources() }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dash, err := tc.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			got, err := SemanticsFromDashboard(dash)
			if err != nil {
				t.Fatalf("semantics: %v", err)
			}

			goldenPath := filepath.Join(testdataDir(), "golden", tc.golden)
			if *updateGolden {
				writeGolden(t, goldenPath, got)
				return
			}

			wantData, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run: go test ./... -update)", goldenPath, err)
			}
			var want DashboardSemantics
			if err := json.Unmarshal(wantData, &want); err != nil {
				t.Fatalf("unmarshal golden: %v", err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("generator semantics changed; run: cd tools/grafana-dashboards && go test ./... -update")
			}
		})
	}
}

func writeGolden(t *testing.T, path string, sem DashboardSemantics) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.MarshalIndent(sem, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func chartDashboardsDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "../../charts/coraza-kubernetes-operator/dashboards"
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../../../charts/coraza-kubernetes-operator/dashboards"))
}

func testdataDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "testdata"
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../testdata"))
}
