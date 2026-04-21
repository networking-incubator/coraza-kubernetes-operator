package corerulesetgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

func stderrf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func stderrln(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...)
}

// Options configures CoreRuleSet manifest generation.
type Options struct {
	RulesDir         string
	Version          string
	IgnoreRuleIDs    map[string]struct{}
	IgnorePMFromFile bool
	IncludeTestRule  bool
	RuleSetName      string
	Namespace        string
	DataSourceName   string
	NamePrefix       string
	NameSuffix       string
	DryRun           bool
	SkipSizeCheck    bool
	Stderr           io.Writer

	// IgnoreUnsupportedRules selects an unsupported-rule profile whose IDs
	// are merged into the effective ignore set. Supported values:
	//   "wasm"  — merge IDs from the operator's WASM-unsupported registry
	//   "none"  — do not merge any unsupported-rule IDs
	// Empty string is treated as "none". Future profiles (e.g. "ext_proc")
	// can be added without changing the flag surface.
	IgnoreUnsupportedRules string

	// autoIgnoredIDs is populated by Build/mergeUnsupportedIDs: rule IDs
	// dropped due to a profile merge but not present in the user's
	// --ignore-rules. Used for clearer warnings in processFileContent.
	autoIgnoredIDs map[string]struct{}
}

// Result holds a short summary after a successful Generate.
type Result struct {
	RuleSourceCount int
	HasDataSource   bool
	RuleSetName     string
	Namespace       string
}

func applyDefaults(opts Options) Options {
	if opts.RuleSetName == "" {
		opts.RuleSetName = "default-ruleset"
	}
	if opts.DataSourceName == "" {
		opts.DataSourceName = "coreruleset-data"
	}
	return opts
}

// Generate walks RulesDir and writes multi-document YAML to out (stdout).
func Generate(out io.Writer, opts Options) (*Result, error) {
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	opts = applyDefaults(opts)

	if err := validateResourceNames(opts); err != nil {
		return nil, err
	}

	if opts.DryRun {
		stderrln(opts.Stderr, "dry-run: no objects sent to cluster")
	}

	ver, err := ParseCRSVersion(opts.Version)
	if err != nil {
		return nil, err
	}

	rulesPath := filepath.Clean(opts.RulesDir)
	st, err := os.Stat(rulesPath)
	if err != nil {
		return nil, fmt.Errorf("rules directory: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("path is not a directory: %s", rulesPath)
	}

	scan, err := Scan(rulesPath)
	if err != nil {
		return nil, err
	}

	bundle, err := Build(opts, scan, ver)
	if err != nil {
		return nil, err
	}

	stderrf(opts.Stderr, "Found %d .conf files in %s\n", len(scan.ConfPaths), rulesPath)
	if len(scan.DataPaths) > 0 {
		stderrf(opts.Stderr, "Found %d .data files in %s\n", len(scan.DataPaths), rulesPath)
	}

	stderrf(opts.Stderr, "\nProcessing %d rule files...\n\n", len(scan.ConfPaths))

	for _, r := range bundle.ConfFileResults {
		stderrf(opts.Stderr, "Processing: %s\n", r.BaseName)
		for _, w := range r.Warns {
			stderrf(opts.Stderr, "%s", w)
		}
		if r.YAML != "" {
			stderrf(opts.Stderr, "  [ok] Generated RuleSource: %s\n", r.SourceName)
		} else {
			stderrf(opts.Stderr, "  [skip] Skipped: %s\n", r.SkipReason)
		}
	}

	if len(scan.DataPaths) > 0 {
		stderrf(opts.Stderr, "\nProcessing %d data files...\n\n", len(scan.DataPaths))
		for _, p := range scan.DataPaths {
			stderrf(opts.Stderr, "Processing: %s\n", filepath.Base(p))
			stderrf(opts.Stderr, "  [ok] Added to RuleData: %s\n", opts.DataSourceName)
		}
	}

	writeGenerateSummary(opts.Stderr, scan.PMFromFileRefs && !opts.IgnorePMFromFile, len(scan.DataPaths) == 0, bundle.Stats.Processed, bundle.Stats.Skipped, len(bundle.ExtraRuleSources), len(scan.DataPaths), opts.DataSourceName)

	if err := WriteManifests(out, bundle); err != nil {
		return nil, err
	}

	stderrf(opts.Stderr, "generated RuleSet %q", opts.RuleSetName)
	if opts.Namespace != "" {
		stderrf(opts.Stderr, " in namespace %q", opts.Namespace)
	}
	stderrf(opts.Stderr, ": %d RuleSource(s), dataSource=%v\n", len(bundle.ExtraRuleSources)+1, len(scan.DataPaths) > 0)

	return &Result{
		RuleSourceCount: len(bundle.ExtraRuleSources) + 1,
		HasDataSource:   len(scan.DataPaths) > 0,
		RuleSetName:     opts.RuleSetName,
		Namespace:       opts.Namespace,
	}, nil
}

func writeGenerateSummary(stderr io.Writer, pmFromFileRefs, noDataFiles bool, processed, skipped, ruleSourceCount, dataFileCount int, dataSourceName string) {
	if pmFromFileRefs && noDataFiles {
		stderrln(stderr, "warning: @pmFromFile references found under the rules directory but no .data files were emitted into a RuleData; add matching .data files or use --ignore-pmFromFile if the operator should not load pmFromFile data.")
	}
	stderrf(stderr, "\n%s\n", strings.Repeat("=", 60))
	stderrln(stderr, "Summary:")
	stderrln(stderr, "  Base rules: 1 (bundled)")
	stderrf(stderr, "  Processed: %d rule files\n", processed)
	stderrf(stderr, "  Skipped: %d rule files\n", skipped)
	stderrf(stderr, "  Total RuleSources: %d\n", ruleSourceCount+1)
	stderrf(stderr, "  Data files: %d\n", dataFileCount)
	if dataFileCount > 0 {
		stderrf(stderr, "  RuleData: %s\n", dataSourceName)
	}
	stderrf(stderr, "%s\n\n", strings.Repeat("=", 60))
}

// validateResourceNames checks that user-provided Kubernetes resource names
// and namespace are valid before generating any manifests.
func validateResourceNames(opts Options) error {
	for _, check := range []struct {
		value, label string
	}{
		{opts.RuleSetName, "ruleset-name"},
		{opts.DataSourceName, "data-source-name"},
	} {
		if errs := validation.IsDNS1123Subdomain(check.value); len(errs) > 0 {
			return fmt.Errorf("invalid %s %q: %s", check.label, check.value, strings.Join(errs, "; "))
		}
	}
	if opts.Namespace != "" {
		if errs := validation.IsDNS1123Label(opts.Namespace); len(errs) > 0 {
			return fmt.Errorf("invalid namespace %q: %s", opts.Namespace, strings.Join(errs, "; "))
		}
	}
	return nil
}
