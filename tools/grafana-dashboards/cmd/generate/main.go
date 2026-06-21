// Command generate writes Grafana dashboard JSON for the Coraza operator Helm chart.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/grafana/grafana-foundation-sdk/go/dashboard"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/grafana-dashboards/internal/dashboards"
)

const defaultOut = "../../charts/coraza-kubernetes-operator/dashboards"

func main() {
	out := flag.String("out", defaultOut, "output directory for dashboard JSON")
	flag.Parse()

	outDir, err := filepath.Abs(*out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve output dir: %v\n", err)
		os.Exit(1)
	}

	if err := generateDashboard(outDir, "coraza-operator-overview.json", dashboards.BuildOverview); err != nil {
		os.Exit(1)
	}
	if err := generateDashboard(outDir, "coraza-operator-resources.json", dashboards.BuildResources); err != nil {
		os.Exit(1)
	}
	if err := generateDashboard(outDir, "coraza-waf-dataplane.json", dashboards.BuildDataplane); err != nil {
		os.Exit(1)
	}
}

func generateDashboard(outDir, name string, build func() (dashboard.Dashboard, error)) error {
	dash, err := build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build %s: %v\n", name, err)
		return err
	}
	path := filepath.Join(outDir, name)
	if err := writeDashboard(path, dash); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
		return err
	}
	fmt.Printf("wrote %s\n", path)
	return nil
}

func writeDashboard(path string, dashboard any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(dashboard, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
