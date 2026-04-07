package corerulesetgen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func injectNamespaceInBaseRuleSourceYAML(doc, ns string) string {
	if ns == "" {
		return doc
	}
	const old = "metadata:\n  name: base-rules"
	const newHead = "metadata:\n  namespace: "
	if !strings.Contains(doc, old) {
		return doc
	}
	return strings.Replace(doc, old, newHead+ns+"\n  name: base-rules", 1)
}

// baseRulesYAML returns the base-rules RuleSource document and the rules scalar (for size checks only).
func baseRulesYAML(normalizedVersion, crsSetupVersion string, includeTest bool) (yamlDoc string, rulesScalar string) {
	inner := fmt.Sprintf(`    SecRuleEngine On
    SecRequestBodyAccess On
    SecRequestBodyLimit 13107200
    SecRequestBodyInMemoryLimit 131072
    SecRequestBodyLimitAction Reject
    SecResponseBodyAccess On
    SecResponseBodyMimeType text/plain text/html text/xml application/json
    # SecRequestBodyJsonDepthLimit directive requires Coraza 3.4.0 on WASM Plugin and it is not supported yet
    # SecRequestBodyJsonDepthLimit 1024
    SecResponseBodyLimit 524288
    SecResponseBodyLimitAction ProcessPartial
    SecAuditEngine RelevantOnly
    SecAuditLogType Serial
    SecAuditLog /dev/stdout
    SecAuditLogFormat JSON
    SecAuditLogParts ABIJDEFHZ
    SecAuditLogRelevantStatus "^(40[0-3]|40[5-9]|4[1-9][0-9]|5[0-9][0-9])$"
    SecDefaultAction "phase:1,log,auditlog,pass"
    SecDefaultAction "phase:2,log,auditlog,pass"
    SecRule REQUEST_HEADERS:Content-Type "^(?:application(?:/soap\+|/)|text/)xml" \
     "id:200000,\
     phase:1,\
     t:none,t:lowercase,\
     pass,\
     nolog,\
     ctl:requestBodyProcessor=XML"
    SecRule REQUEST_HEADERS:Content-Type "^application/json" \
     "id:200001,\
     phase:1,\
     t:none,t:lowercase,\
     pass,\
     nolog,\
     ctl:requestBodyProcessor=JSON"
    SecRule REQUEST_HEADERS:Content-Type "^application/[a-z0-9.-]+\+json" \
     "id:200006,\
     phase:1,\
     t:none,t:lowercase,\
     pass,\
     nolog,\
     ctl:requestBodyProcessor=JSON"
    SecRule REQBODY_ERROR "!@eq 0" \
     "id:200002,\
     phase:2,\
     t:none,\
     log,\
     deny,\
     status:400,\
     msg:'Failed to parse request body.',\
     logdata:'%%{reqbody_error_msg}',\
     severity:2"
    SecRule MULTIPART_STRICT_ERROR "!@eq 0" \
     "id:200003,\
     phase:2,\
     t:none,\
     log,\
     deny,\
     status:400,\
     msg:'Multipart request body failed strict validation.'"
    SecAction \
     "id:900120,\
     phase:1,\
     pass,\
     t:none,\
     nolog,\
     tag:'OWASP_CRS',\
     ver:'OWASP_CRS/%s',\
     setvar:tx.early_blocking=1"
    SecAction \
     "id:900990,\
     phase:1,\
     pass,\
     t:none,\
     nolog,\
     tag:'OWASP_CRS',\
     ver:'OWASP_CRS/%s',\
     setvar:tx.crs_setup_version=%s"
`, normalizedVersion, normalizedVersion, crsSetupVersion)
	inner = strings.TrimRight(inner, "\n")

	body := "apiVersion: waf.k8s.coraza.io/v1alpha1\nkind: RuleSource\nmetadata:\n  name: base-rules\nspec:\n  type: Rule\n  rules: |\n" + inner
	s := strings.TrimRight(body, "\n")
	if includeTest {
		s += "\n" + xCRSTestBlock
	}

	rulesScalar = inner + "\n"
	if includeTest {
		rulesScalar += xCRSTestBlock + "\n"
	}
	return s, rulesScalar
}

const xCRSTestBlock = `    SecResponseBodyMimeType text/plain text/html text/xml application/json
    SecDefaultAction "phase:3,log,auditlog,pass"
    SecDefaultAction "phase:4,log,auditlog,pass"
    SecDefaultAction "phase:5,log,auditlog,pass"
    SecDebugLogLevel 3
    SecAction \
     "id:900005,\
     phase:1,\
     nolog,\
     pass,\
     t:none,\
     setvar:tx.blocking_paranoia_level=4,\
     setvar:tx.crs_validate_utf8_encoding=1,\
     setvar:tx.arg_name_length=100,\
     setvar:tx.arg_length=400,\
     setvar:tx.total_arg_length=64000,\
     setvar:tx.max_num_args=255,\
     setvar:tx.max_file_size=64100,\
     setvar:tx.combined_file_sizes=65535,\
     ctl:ruleEngine=DetectionOnly,\
     ctl:ruleRemoveById=910000"
    SecRule REQUEST_HEADERS:X-CRS-Test "@rx ^.*$" \
     "id:999999,\
     pass,\
     phase:1,\
     log,\
     msg:'X-CRS-Test %{MATCHED_VAR}',\
     ctl:ruleRemoveById=1-999999"`

// indentMultiline prefixes each line with indent spaces for YAML block scalars.
// indent must exceed the column of the block's mapping key so the scalar is valid YAML.
func indentMultiline(processed string, indent int) string {
	prefix := strings.Repeat(" ", indent)
	var b strings.Builder
	for line := range strings.SplitSeq(processed, "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString(prefix)
			b.WriteByte('\n')
		} else {
			b.WriteString(prefix)
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func buildDataRuleSourceYAML(dataFiles []string, opts Options) (string, error) {
	entries := make(map[string]string, len(dataFiles))
	for _, p := range dataFiles {
		key := filepath.Base(p)
		if err := validateDataFileKey(key); err != nil {
			return "", fmt.Errorf("data file %s: %w", p, err)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read data file %s: %w", p, err)
		}
		entries[key] = strings.ToValidUTF8(string(raw), "")
	}
	if err := checkRuleSourceDataSize(opts.DataSourceName, entries, opts); err != nil {
		return "", err
	}
	return formatDataRuleSourceYAML(opts.DataSourceName, opts.Namespace, entries), nil
}

func rulesetYAML(sourceNames []string, opts Options, includeData bool) string {
	dataSourceName := ""
	if includeData {
		dataSourceName = opts.DataSourceName
	}
	return formatRuleSetYAML(opts.RuleSetName, opts.Namespace, sourceNames, dataSourceName)
}

func formatRuleSourceYAML(name, namespace, indentedRules string) string {
	var b strings.Builder
	b.WriteString("apiVersion: waf.k8s.coraza.io/v1alpha1\nkind: RuleSource\nmetadata:\n")
	if namespace != "" {
		fmt.Fprintf(&b, "  namespace: %s\n", namespace)
	}
	fmt.Fprintf(&b, "  name: %s\nspec:\n  type: Rule\n  rules: |\n%s\n", name, indentedRules)
	return b.String()
}

func formatDataRuleSourceYAML(name, namespace string, files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("apiVersion: waf.k8s.coraza.io/v1alpha1\nkind: RuleSource\nmetadata:\n")
	if namespace != "" {
		fmt.Fprintf(&b, "  namespace: %s\n", namespace)
	}
	fmt.Fprintf(&b, "  name: %s\nspec:\n  type: Data\n  files:\n", name)
	for _, k := range keys {
		// Keys are under spec.files at 4 spaces; block lines must be indented deeper than the key.
		indented := indentMultiline(files[k], 6)
		fmt.Fprintf(&b, "    %s: |\n%s\n", k, indented)
	}
	return b.String()
}

func formatRuleSetYAML(rulesetName, namespace string, sourceNames []string, dataSourceName string) string {
	var b strings.Builder
	b.WriteString("apiVersion: waf.k8s.coraza.io/v1alpha1\nkind: RuleSet\nmetadata:\n")
	if namespace != "" {
		fmt.Fprintf(&b, "  namespace: %s\n", namespace)
	}
	fmt.Fprintf(&b, "  name: %s\nspec:\n  sources:\n", rulesetName)
	if dataSourceName != "" {
		fmt.Fprintf(&b, "    - name: %s\n", dataSourceName)
	}
	b.WriteString("    - name: base-rules\n")
	for _, n := range sourceNames {
		fmt.Fprintf(&b, "    - name: %s\n", n)
	}
	return b.String()
}
