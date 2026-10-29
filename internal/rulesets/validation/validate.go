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

// Package validation provides Coraza validation helpers for rule text.
package validation

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/corazawaf/coraza/v3"
	"github.com/corazawaf/coraza/v3/experimental"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/memfs"
)

// validationTrailer pins the validation WAF's audit writer to a stream. Coraza
// opens the SecAuditLog target while compiling a WAF, regardless of
// SecAuditEngine, and the resulting writer is not reachable from the public WAF
// interface, so nothing can close it: a file target such as /dev/null leaks one
// descriptor per validation. The serial writer special-cases the streams and
// opens no descriptor at all. SecAuditLogType is pinned so a tenant Concurrent
// or Syslog writer cannot open one either. The delivered configuration is
// unaffected.
const validationTrailer = "\nSecAuditLogType Serial\nSecAuditLog /dev/stdout"

// BuildWAFConfig returns a validation WAF config whose root filesystem is
// always in-memory. Coraza falls back to io.OSFS when no root is set, so tenant
// rule text could otherwise read operator files (an Include of the ServiceAccount
// token leaks it through the compilation error into tenant-readable status).
// Runtime include aliases and embedded CRS data files are stubbed, in the rule
// text and in every RuleData file it Includes, so Coraza does not reject what
// the plugin resolves from its embedded filesystem.
func BuildWAFConfig(rules string, dataFiles map[string][]byte) coraza.WAFConfig {
	mfs := memfs.NewMemFS()
	for name, data := range dataFiles {
		mfs.WriteFile(name, data)
	}
	for _, target := range validationStubTargets(rules, dataFiles) {
		if _, ok := dataFiles[target]; !ok {
			mfs.WriteFile(target, nil)
		}
	}
	return coraza.NewWAFConfig().WithDirectives(rules + validationTrailer).WithRootFS(mfs)
}

// CloseWAF releases a validation WAF. Coraza registers the WAF's compiled-pattern
// memoizer at NewWAF and only Close releases it, so every validation build must
// call this or leak its entry.
func CloseWAF(waf coraza.WAF) {
	if closer, ok := waf.(experimental.WAFCloser); ok {
		_ = closer.Close()
	}
}

// validationStubTargets returns the runtime include aliases and embedded
// @pmFromFile data files the plugin resolves from its own filesystem. Coraza
// parses an included RuleData file as rule text, so the aliases and @pmf
// targets it contains must be stubbed too: follow the include chain exactly as
// Coraza does, relative to the including file's directory.
func validationStubTargets(rules string, dataFiles map[string][]byte) []string {
	var targets []string
	queue := []struct{ name, text string }{{text: rules}}
	seen := make(map[string]struct{}, len(dataFiles))
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		targets = append(targets, runtimeIncludeTargets(current.text)...)
		targets = append(targets, embeddedDataTargets(current.text)...)
		for _, name := range ruleDataIncludeNames(current.text) {
			name = path.Join(path.Dir(current.name), name)
			if _, ok := seen[name]; ok {
				continue
			}
			data, ok := dataFiles[name]
			if !ok {
				continue
			}
			seen[name] = struct{}{}
			queue = append(queue, struct{ name, text string }{name: name, text: string(data)})
		}
	}
	return targets
}

// includeTarget returns the Include argument exactly as Coraza's evaluateLine
// produces it: the text after the first single space, with every leading and
// trailing double quote stripped when the argument is wholly quoted and at
// least three bytes long (strings.Trim, never a single quote). FromFile decides
// glob vs literal on this value: it expands a glob with the argument untrimmed
// and trims only the literal name it opens (internal/seclang/parser.go). The
// guard and the stubs must agree with that, or they resolve a name the plugin
// reads under a different one: the guard then never scans the included file,
// and Coraza still opens it.
func includeTarget(arg string) string {
	if len(arg) >= 3 && arg[0] == '"' && arg[len(arg)-1] == '"' {
		return strings.Trim(arg, "\"")
	}
	return arg
}

// ruleDataIncludeNames returns the RuleData file names a rule text Includes.
// Aliases, globs and filesystem paths are omitted: aliases and globs are
// stubbed as targets, and filesystem paths are rejected before compilation.
func ruleDataIncludeNames(rules string) []string {
	var names []string
	for line := range strings.SplitSeq(joinActiveRuleLines(rules), "\n") {
		match := includePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		target := includeTarget(match[1])
		if isGlob(target) {
			continue
		}
		target = strings.TrimSpace(target)
		if isOSPath(target) || strings.HasPrefix(target, "@") {
			continue
		}
		names = append(names, path.Clean(target))
	}
	return names
}

// runtimeIncludeTargets returns the non-glob Include targets resolved by the
// plugin at runtime, so validation can stub them.
func runtimeIncludeTargets(rules string) []string {
	var targets []string
	for line := range strings.SplitSeq(joinActiveRuleLines(rules), "\n") {
		match := includePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		target := strings.TrimSpace(includeTarget(match[1]))
		if isGlob(target) {
			if target == "crs/rules/*.conf" {
				targets = append(targets, "crs/rules/000-stub.conf")
			}
			continue
		}
		if isRuntimeResolvedPath(target) {
			targets = append(targets, target)
		}
	}
	return targets
}

// embeddedDataTargets returns the @pmFromFile targets the plugin resolves from
// its embedded CRS tree, so validation can stub them.
func embeddedDataTargets(rules string) []string {
	var targets []string
	for _, match := range pmFromFilePattern.FindAllStringSubmatch(joinActiveRuleLines(rules), -1) {
		if len(match) < 3 {
			continue
		}
		target := path.Clean(strings.Trim(strings.TrimSpace(match[2]), `"'`))
		if isEmbeddedDataPath(target) {
			targets = append(targets, target)
		}
	}
	return targets
}

// missingFileWrappedPattern matches a missing-file read error whose error chain
// is lost because Coraza wraps it with %s instead of %w ("failed to readfile:
// open <path>: file does not exist").
var missingFileWrappedPattern = regexp.MustCompile(`failed to readfile: open (.+?): (?:file does not exist|no such file or directory)`)

// ExtractMissingFilePath returns the path from a missing-file error. It walks
// the error chain for *fs.PathError with fs.ErrNotExist and falls back to the
// text Coraza produces when it wraps the read error with %s.
func ExtractMissingFilePath(err error) (string, bool) {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && errors.Is(pathErr.Err, fs.ErrNotExist) {
		return pathErr.Path, true
	}
	if match := missingFileWrappedPattern.FindStringSubmatch(err.Error()); match != nil {
		return match[1], true
	}
	return "", false
}

// ExtractMissingFileBasename extracts the base filename from a missing-file
// error. See ExtractMissingFilePath.
func ExtractMissingFileBasename(err error) (string, bool) {
	name, ok := ExtractMissingFilePath(err)
	if !ok {
		return "", false
	}
	return filepath.Base(name), true
}

// isRuntimeResolvedPath reports whether the plugin's embedded filesystem
// resolves the path at runtime. Only these aliases are resolvable there: any
// other @-prefixed target passes validation but makes the plugin NACK every
// push, which the operator cannot report because the Engine looks Ready.
// The four file aliases name a single embedded file. @owasp_crs/<path> maps to
// the embedded CRS tree, so the path must name a rule file inside it: the bare
// @owasp_crs and a path with an empty, "." or ".." segment name the directory
// itself, which cannot be included, and a literal must end in .conf, the
// extension of every embedded rule file. The other files in that tree are not
// rule text: Coraza fails to parse the .data files ("unknown directive", so the
// plugin NACKs) and the .conf.example templates hold no active rules (a
// zero-rule WAF). Neither produces a working WAF while the Engine looks Ready,
// so both are rejected. A glob only works where the embedded tree holds the files
// and every match is valid rule text. The plugin expands it with the CRS
// directory's ReadDir, and Coraza only logs an empty glob result, so a pattern
// that matches nothing silently yields a zero-rule WAF. The exact unpadded
// @owasp_crs/*.conf is the only glob the embedded tree guarantees to match
// (every embedded CRS rule file ends in .conf): any other pattern is rejected
// because the operator cannot enumerate what it matches, not because it fails:
// @owasp_crs/REQUEST-*.conf compiles rules, @owasp_crs/*.data NACKs, and
// @owasp_crs/NOSUCH-*.conf or a nested @owasp_crs/crs/*.conf compiles a
// zero-rule WAF. A wrong literal file name there is a documented delivery risk:
// the operator cannot verify the embedded file list.
func isRuntimeResolvedPath(target string) bool {
	switch target {
	case "@recommended-conf", "@demo-conf", "@ftw-conf", "@crs-setup-conf":
		return true
	}
	if rel, ok := strings.CutPrefix(target, "crs/"); ok {
		// coraza-rs embeds the pinned CRS under crs/: the setup file and the
		// rule files in crs/rules/. Only .conf files are rule text.
		if target == "crs/crs-setup.conf" {
			return true
		}
		if rel == "rules/*.conf" {
			return true
		}
		return strings.HasPrefix(rel, "rules/") && strings.HasSuffix(rel, ".conf") && !isGlob(rel)
	}
	rel, ok := strings.CutPrefix(target, "@owasp_crs/")
	if !ok {
		return false
	}
	elems := strings.Split(rel, "/")
	for _, elem := range elems {
		if elem == "" || elem == "." || elem == ".." {
			return false
		}
	}
	// Only the exact all-conf glob is guaranteed to match embedded rule files,
	// and it must be the only segment: a nested glob names a directory the
	// plugin does not have, so ReadDir fails, Coraza logs an empty glob result
	// and the WAF compiles with zero rules.
	if isGlob(rel) {
		return rel == "*.conf"
	}
	// A literal names a rule file, so it must end in .conf. The other embedded
	// files (.conf.example, .data, .txt, ...) compile zero rules or fail to
	// parse as SecLang, and the Engine would still report Ready.
	return strings.HasSuffix(rel, ".conf")
}

// isGlob reports whether elem contains a glob metacharacter. Coraza expands
// Include targets with fs.Glob, whose pattern syntax uses these characters.
func isGlob(elem string) bool {
	return strings.ContainsAny(elem, `*?[\`)
}

// isOSPath reports whether path addresses the operator's filesystem instead of
// a RuleData file name (absolute, parent-relative anywhere in the path, Windows
// volume or UNC). Such paths are always rejected: only RuleData names and
// runtime aliases may be referenced.
func isOSPath(target string) bool {
	if target == "" {
		return false
	}
	p := strings.ReplaceAll(target, `\`, "/")
	if strings.HasPrefix(p, "/") || (len(p) >= 2 && p[1] == ':') { // /... or C:\...
		return true
	}
	for elem := range strings.SplitSeq(p, "/") {
		if elem == ".." {
			return true
		}
	}
	return false
}

// SanitizeErrorMessage replaces filesystem paths in missing-file errors with
// just the base filename to prevent disclosure of container-internal paths.
func SanitizeErrorMessage(err error) error {
	if basename, ok := ExtractMissingFileBasename(err); ok {
		return fmt.Errorf("open %s: data does not exist", basename)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("validation failed: referenced file does not exist (path redacted)")
	}
	return err
}

// ShouldSkipMissingFileError reports whether a missing-file validation error
// should be skipped. A runtime alias (@owasp_crs/..., @crs-setup-conf, ...) is
// always skipped: the plugin's merged filesystem resolves it. Otherwise, when
// dataFiles is nil (RuleSource reconciler path, no RuleSet-scoped RuleData
// merged), any missing-file error is skipped so aggregate RuleSet validation
// remains authoritative for Include targets and @pmFromFile. When dataFiles is
// non-nil, the error is skipped only if basename exists in the map (use an
// empty non-nil map to require resolution without providing files).
func ShouldSkipMissingFileError(err error, dataFiles map[string][]byte) bool {
	name, ok := ExtractMissingFilePath(err)
	if !ok {
		return false
	}
	if isRuntimeResolvedPath(name) {
		return true
	}
	if dataFiles == nil {
		return true
	}
	_, exists := dataFiles[filepath.Base(name)]
	return exists
}

// includePattern matches an Include directive and its whole argument in rule
// text already normalised by joinActiveRuleLines. Coraza cuts the line at the
// first single space and reads the rest as the argument, so only that literal
// space starts it: a tab after "include" is an unknown directive to Coraza, and
// the guard must not resolve it as an Include.
var includePattern = regexp.MustCompile(`(?i)^include (.+)$`)

// forbiddenDirectivePattern matches directives that make Coraza open or create a
// host file while compiling a WAF, regardless of the in-memory root filesystem.
// SecDebugLogLevel is deliberately not matched: it only sets a verbosity and
// opens nothing. SecAuditLog is rejected separately: envoy-coraza has no audit
// sink and rejects any policy that names one.
var forbiddenDirectivePattern = regexp.MustCompile(`(?i)^(secdebuglog|secuploaddir)(?:[ \t]|$)`)

// auditLogPattern matches SecAuditLog and its argument exactly as Coraza passes
// it to the audit writer: the rest of the line after the first space.
// SecAuditLogType is not matched: "secauditlog" must be followed by a space or
// the end of line.
var auditLogPattern = regexp.MustCompile(`(?i)^secauditlog(?: (.*))?$`)

// uploadKeepFilesPattern matches SecUploadKeepFiles and its value. Only "Off" is
// accepted: every other value keeps uploaded files on disk, and the plugin has
// no upload directory.
var uploadKeepFilesPattern = regexp.MustCompile(`(?i)^secuploadkeepfiles(?:[ \t]+(\S+))?`)

// nonRuleCrsTarget reports whether target names a literal file in the embedded
// CRS tree whose extension is not .conf, after the path cleaning the plugin's
// filesystem applies. Coraza parses the target as SecLang: the .data files and
// the .conf.example templates compile zero rules or fail with "unknown
// directive", so the plugin either NACKs the push or enforces nothing while the
// Engine reports Ready (see isRuntimeResolvedPath). Globs are handled by their
// own case; the bare crs directory is matched too, since it is not a file.
func nonRuleCrsTarget(target string) bool {
	target = path.Clean(target)
	var rel string
	switch {
	case strings.HasPrefix(target, "@owasp_crs/"):
		rel = strings.TrimPrefix(target, "@owasp_crs/")
	case target == "crs":
		// The embedded directory itself, never a file.
		return true
	case strings.HasPrefix(target, "crs/"):
		rel = strings.TrimPrefix(target, "crs/")
	default:
		return false
	}
	return !isGlob(rel) && !strings.HasSuffix(rel, ".conf")
}

// allowedIncludeTargets lists the reference forms a tenant may use. It is
// static so neither a rejected path nor rule content can leak into status.
const allowedIncludeTargets = "@recommended-conf, @demo-conf, @ftw-conf, @crs-setup-conf, @owasp_crs/*.conf, @owasp_crs/<file>.conf or a RuleData file name"

// ValidateIncludes rejects Include targets that address the operator
// filesystem, unresolvable aliases, and directives that write host files. The
// validation WAF has an in-memory root, so such a path cannot resolve anyway;
// rejecting it here keeps the error honest and independent of the missing-file
// skip that defers RuleData names to aggregate validation. Runtime aliases
// (@owasp_crs/..., @crs-setup-conf, ...) are accepted because the plugin
// resolves them from its embedded filesystem; a literal CRS-tree path must name
// a .conf rule file (see isRuntimeResolvedPath and nonRuleCrsTarget). Coraza
// trims a non-glob Include target, so a padded target is accepted and resolved
// to the name Coraza opens. It expands a glob from the untrimmed argument, so a
// glob is accepted only as the exact unpadded @owasp_crs/*.conf: a padded
// pattern matches nothing and compiles a zero-rule WAF without an error. A
// target that still carries a quote after the double quotes Coraza strips is
// rejected, because Coraza reads the quote as part of the file name.
//
// dataFiles are the RuleData files merged for the RuleSet. An Include of a
// RuleData name is parsed as rule text, so the content of every included file
// is scanned too. @pmFromFile data is never parsed as SecLang and is not
// scanned: a blocklist that happens to contain the text of a forbidden
// directive is valid. Pass nil on the RuleSource path, which has no merged data.
func ValidateIncludes(rules string, dataFiles map[string][]byte) error {
	queue := []struct{ name, text string }{{text: rules}}
	scanned := make(map[string]struct{}, len(dataFiles))
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		included, err := validateRuleText(current.text)
		if err != nil {
			if current.name == "" {
				return err
			}
			return fmt.Errorf("included RuleData file %s: %w", current.name, err)
		}
		for _, name := range included {
			// Coraza resolves a nested Include relative to the including
			// file's directory, so the guard must scan that same file.
			name = path.Join(path.Dir(current.name), name)
			if _, seen := scanned[name]; seen {
				continue
			}
			data, ok := dataFiles[name]
			if !ok {
				continue
			}
			scanned[name] = struct{}{}
			queue = append(queue, struct{ name, text string }{name: name, text: string(data)})
		}
	}
	return nil
}

// validateRuleText rejects the directives and Include targets a tenant rule
// text (or an included data file) may not use. It returns the RuleData file
// names the text Includes, so the caller can scan their content too.
// shortcut: a glob Include is not expanded for scanning; only the exact
// @owasp_crs/*.conf is accepted, and those files resolve inside the plugin's
// embedded tree, which the operator cannot enumerate.
func validateRuleText(rules string) ([]string, error) {
	// Coraza skips blank and comment lines before checking for a line
	// continuation, so a trailing "\" would swallow the first directive of
	// validationTrailer and report a confusing action error.
	lastContinued := false
	for line := range strings.SplitSeq(rules, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			lastContinued = strings.HasSuffix(line, `\`)
		}
	}
	if lastContinued {
		return nil, errors.New(`rule text ends with a line continuation: remove the trailing "\"`)
	}

	var included []string
	for line := range strings.SplitSeq(joinActiveRuleLines(rules), "\n") {
		if match := forbiddenDirectivePattern.FindStringSubmatch(line); match != nil {
			return nil, fmt.Errorf("%s is not allowed: rule validation must not write host files", match[1])
		}
		if match := uploadKeepFilesPattern.FindStringSubmatch(line); match != nil {
			if len(match) < 2 || !strings.EqualFold(strings.Trim(match[1], `"'`), "off") {
				return nil, errors.New("SecUploadKeepFiles is not allowed: rule validation must not write host files")
			}
		}
		if auditLogPattern.MatchString(line) {
			return nil, errors.New("SecAuditLog is not supported by envoy-coraza: remove it")
		}
		match := includePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		target := includeTarget(match[1])
		// Coraza expands a glob from the untrimmed argument, so a padded glob
		// matches nothing and compiles a zero-rule WAF without an error. It
		// trims only the literal name it opens, so a padded non-glob resolves
		// normally.
		if strings.Contains(target, "*") {
			if target != strings.TrimSpace(target) {
				return nil, errors.New(`include of a glob must not be padded: Coraza expands the glob before trimming, so padding makes it match nothing; use ` + allowedIncludeTargets)
			}
		} else {
			target = strings.TrimSpace(target)
		}
		switch {
		case strings.ContainsAny(target, `"'`):
			return nil, errors.New(`include target must not contain quotes: only double quotes around the whole target are stripped, use ` + allowedIncludeTargets)
		case isOSPath(target):
			return nil, errors.New(`include with an absolute or parent-relative path is not allowed: use ` + allowedIncludeTargets)
		case nonRuleCrsTarget(target):
			return nil, errors.New(`include of a non-rule file from the embedded CRS tree is not allowed: use @owasp_crs/*.conf or @owasp_crs/<file>.conf`)
		case strings.HasPrefix(target, "@"):
			if !isRuntimeResolvedPath(target) {
				return nil, errors.New(`include alias is not resolved by the plugin: use ` + allowedIncludeTargets)
			}
		case isRuntimeResolvedPath(target):
			// Embedded coraza-rs CRS tree; not a RuleData name.
		case isGlob(target):
			// The validation WAF's root cannot expand a glob, and the plugin
			// also expands crs/ patterns, so only the exact @owasp_crs/*.conf
			// form is safe (see isRuntimeResolvedPath). Any other glob is
			// rejected: the operator cannot tell whether it matches rule files,
			// nothing at all (a zero-rule WAF) or a .data file (a NACK).
			return nil, errors.New(`include with a glob is only allowed as @owasp_crs/*.conf: use ` + allowedIncludeTargets)
		default:
			included = append(included, path.Clean(target))
		}
	}
	return included, nil
}

// ValidateRuleSourceRules validates SecLang rule text via Coraza. dataFiles
// supplies in-memory files for Include and @pmFromFile resolution. nil means no
// files are provided: missing-file errors are skipped (RuleSource controller
// path). Pass a non-nil map (including an empty map) to require that missing
// files appear in the map. RuleSet aggregate validation remains authoritative
// for cross-source and full RuleData-backed rules.
func ValidateRuleSourceRules(rules, ruleSourceName string, dataFiles map[string][]byte) error {
	// The guard is cheap and applies to patch-only fragments too: they are
	// skipped by Coraza, not by the directive and alias rules.
	if err := ValidateIncludes(rules, dataFiles); err != nil {
		return fmt.Errorf("RuleSource %s doesn't contain valid rules: %w", ruleSourceName, err)
	}
	if IsPatchOnlyFragment(rules) {
		return nil
	}
	waf, err := coraza.NewWAF(BuildWAFConfig(rules, dataFiles))
	if err != nil {
		if ShouldSkipMissingFileError(err, dataFiles) {
			return nil
		}
		return fmt.Errorf("RuleSource %s doesn't contain valid rules: %w", ruleSourceName, SanitizeErrorMessage(err))
	}
	CloseWAF(waf)
	return nil
}
