/*
Copyright Coraza Kubernetes Operator contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// kubectl-coraza is a kubectl plugin (kubectl coraza …) for generating RuleSet-related
// manifests from OWASP CoreRuleSet files on disk. It does not compile rules; the
// operator validates and compiles after apply.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/networking-incubator/coraza-kubernetes-operator/tools/corerulesetgen"
)

// -----------------------------------------------------------------------------
// Vars
// -----------------------------------------------------------------------------

var (
	version = "v0.0.0-dev"
)

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func main() {
	root := &cobra.Command{
		Use:   "kubectl-coraza",
		Short: "kubectl plugin for Coraza Kubernetes operator utilities",
		Long: `Client-side generators for manifests consumed by the Coraza Kubernetes operator.
Validation and Coraza compilation happen in the operator after you apply resources.`,
		Version: version,
	}
	root.CompletionOptions.DisableDefaultCmd = true

	generate := &cobra.Command{
		Use:   "generate",
		Short: "Generate Kubernetes manifests",
	}
	coreruleset := &cobra.Command{
		Use:   "coreruleset",
		Short: "Emit ConfigMaps, optional coraza/data Secret, and RuleSet from CoreRuleSet .conf/.data files",
		Long: `Reads *.conf and *.data from a single directory (non-recursive), matching the behavior of the
former Makefile Python generator. Output is a multi-document YAML stream on stdout.

RuleSet references ConfigMaps in the same namespace as the RuleSet; set --namespace when you
need metadata.namespace on every object.`,
		RunE: genCRS,
	}

	flags := coreruleset.Flags()
	flags.String("rules-dir", "", "directory containing CoreRuleSet *.conf (and optional *.data) [required]")
	_ = coreruleset.MarkFlagRequired("rules-dir")
	flags.String("version", "", "CoreRuleSet version (e.g. 4.24.1 or v4.24.1) [required]")
	_ = coreruleset.MarkFlagRequired("version")
	flags.String("ignore-rules", "", "comma-separated rule IDs to drop")
	flags.Bool("ignore-pmFromFile", false, "strip SecRule lines that use @pmFromFile")
	flags.Bool("include-test-rule", false, "append X-CRS-Test block to the bundled base-rules ConfigMap")
	flags.String("ruleset-name", "default-ruleset", "metadata.name of the RuleSet")
	flags.StringP("namespace", "n", "", "if set, metadata.namespace on all generated objects")
	flags.String("data-secret-name", "coreruleset-data", "Secret name for *.data files (type coraza/data)")
	flags.String("name-prefix", "", "optional prefix for ConfigMap names derived from *.conf filenames (not base-rules)")
	flags.String("name-suffix", "", "optional suffix for ConfigMap names derived from *.conf filenames")
	flags.String("dry-run", "", "if set to client, print the same manifests and annotate stderr (no cluster access is performed either way)")
	flags.Bool("skip-size-check", false, "allow very large rules payloads (not recommended; etcd limits may still reject applies)")

	root.AddCommand(generate)
	generate.AddCommand(coreruleset)

	root.InitDefaultVersionFlag()
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// -----------------------------------------------------------------------------
// Generator
// -----------------------------------------------------------------------------

func genCRS(cmd *cobra.Command, _ []string) error {
	flags := cmd.Flags()
	rulesDir, _ := flags.GetString("rules-dir")
	ver, _ := flags.GetString("version")
	ignoreCSV, _ := flags.GetString("ignore-rules")
	ignorePM, _ := flags.GetBool("ignore-pmFromFile")
	includeTest, _ := flags.GetBool("include-test-rule")
	rulesetName, _ := flags.GetString("ruleset-name")
	namespace, _ := flags.GetString("namespace")
	dataSecret, _ := flags.GetString("data-secret-name")
	namePrefix, _ := flags.GetString("name-prefix")
	nameSuffix, _ := flags.GetString("name-suffix")
	dry, _ := flags.GetString("dry-run")
	skipSize, _ := flags.GetBool("skip-size-check")

	ignoreSet := map[string]struct{}{}
	if strings.TrimSpace(ignoreCSV) != "" {
		for p := range strings.SplitSeq(ignoreCSV, ",") {
			id := strings.TrimSpace(p)
			if id != "" {
				ignoreSet[id] = struct{}{}
			}
		}
		if len(ignoreSet) > 0 {
			ids := make([]string, 0, len(ignoreSet))
			for id := range ignoreSet {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			fmt.Fprintf(os.Stderr, "Ignoring rule IDs: %s\n", strings.Join(ids, ", "))
		}
	}

	opts := corerulesetgen.Options{
		RulesDir:         rulesDir,
		Version:          ver,
		IgnoreRuleIDs:    ignoreSet,
		IgnorePMFromFile: ignorePM,
		IncludeTestRule:  includeTest,
		RuleSetName:      rulesetName,
		Namespace:        namespace,
		DataSecretName:   dataSecret,
		NamePrefix:       namePrefix,
		NameSuffix:       nameSuffix,
		DryRun:           strings.EqualFold(strings.TrimSpace(dry), "client"),
		SkipSizeCheck:    skipSize,
		Stderr:           os.Stderr,
	}

	_, err := corerulesetgen.Generate(cmd.OutOrStdout(), opts)
	return err
}
