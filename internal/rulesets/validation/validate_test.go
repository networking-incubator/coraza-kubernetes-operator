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

package validation

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/corazawaf/coraza/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractMissingFileBasename(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantBase string
		wantOK   bool
	}{
		{
			name:     "bare PathError with ErrNotExist",
			err:      &fs.PathError{Op: "open", Path: "/var/lib/data/rules.conf", Err: fs.ErrNotExist},
			wantBase: "rules.conf",
			wantOK:   true,
		},
		{
			name:     "single-wrapped PathError",
			err:      fmt.Errorf("outer: %w", &fs.PathError{Op: "open", Path: "/secret/path/file.data", Err: fs.ErrNotExist}),
			wantBase: "file.data",
			wantOK:   true,
		},
		{
			name: "double-wrapped PathError (Coraza style)",
			err: fmt.Errorf("invalid WAF config from string: %w",
				fmt.Errorf("failed to compile the directive \"secrule\": %w",
					&fs.PathError{Op: "open", Path: "/app/rules/modsec.data", Err: fs.ErrNotExist})),
			wantBase: "modsec.data",
			wantOK:   true,
		},
		{
			name:     "relative path",
			err:      &fs.PathError{Op: "open", Path: "relative/file.txt", Err: fs.ErrNotExist},
			wantBase: "file.txt",
			wantOK:   true,
		},
		{
			name:     "PathError Op stat (not just open)",
			err:      &fs.PathError{Op: "stat", Path: "/var/lib/waf/stats.data", Err: fs.ErrNotExist},
			wantBase: "stats.data",
			wantOK:   true,
		},
		{
			name:     "PathError with syscall.ENOENT (Unix)",
			err:      &fs.PathError{Op: "open", Path: "/etc/coraza/x.data", Err: syscall.ENOENT},
			wantBase: "x.data",
			wantOK:   true,
		},
		{
			name:     "PathError with different Err (not ErrNotExist)",
			err:      &fs.PathError{Op: "open", Path: "/some/path", Err: fs.ErrPermission},
			wantBase: "",
			wantOK:   false,
		},
		{
			name:     "non-PathError",
			err:      errors.New("completely unrelated error"),
			wantBase: "",
			wantOK:   false,
		},
		{
			name:     "ErrNotExist without PathError wrapper",
			err:      fmt.Errorf("something went wrong: %w", fs.ErrNotExist),
			wantBase: "",
			wantOK:   false,
		},
		{
			name:     "Coraza %s-wrapped read error",
			err:      errors.New("invalid WAF config from string: failed to readfile: open blocklist.conf: file does not exist"),
			wantBase: "blocklist.conf",
			wantOK:   true,
		},
		{
			name:     "Coraza %s-wrapped read error with an absolute path",
			err:      errors.New("invalid WAF config from string: failed to readfile: open /home/operator/waf/nope.data: no such file or directory"),
			wantBase: "nope.data",
			wantOK:   true,
		},
		{
			name:     "Coraza %s-wrapped parse error is not a missing file",
			err:      errors.New(`failed to parse string: failed to compile the directive "secrule": invalid action`),
			wantBase: "",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, ok := ExtractMissingFileBasename(tt.err)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantBase, base)
		})
	}
}

func TestSanitizeErrorMessage(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantSubstring string
		wantNoAbsPath bool
	}{
		{
			name:          "PathError sanitized to basename",
			err:           &fs.PathError{Op: "open", Path: "/var/lib/operator/secret.data", Err: fs.ErrNotExist},
			wantSubstring: "open secret.data: data does not exist",
			wantNoAbsPath: true,
		},
		{
			name: "wrapped PathError sanitized",
			err: fmt.Errorf("invalid WAF config: %w",
				&fs.PathError{Op: "open", Path: "/deep/nested/path/file.conf", Err: fs.ErrNotExist}),
			wantSubstring: "open file.conf: data does not exist",
			wantNoAbsPath: true,
		},
		{
			name:          "ErrNotExist without PathError gets generic redaction",
			err:           fmt.Errorf("something: %w", fs.ErrNotExist),
			wantSubstring: "referenced file does not exist (path redacted)",
			wantNoAbsPath: true,
		},
		{
			name:          "Coraza %s-wrapped read error sanitized to basename",
			err:           errors.New("invalid WAF config from string: failed to readfile: open /home/operator/waf/validation/nope.data: file does not exist"),
			wantSubstring: "open nope.data: data does not exist",
			wantNoAbsPath: true,
		},
		{
			name:          "Coraza %s-wrapped no such file error sanitized to basename",
			err:           errors.New("failed to parse string: failed to readfile: open blocklist.conf: no such file or directory"),
			wantSubstring: "open blocklist.conf: data does not exist",
			wantNoAbsPath: true,
		},
		{
			name:          "non-file error passes through",
			err:           errors.New("syntax error in directive"),
			wantSubstring: "syntax error in directive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SanitizeErrorMessage(tt.err)
			assert.Contains(t, result.Error(), tt.wantSubstring)
			if tt.wantNoAbsPath {
				assert.False(t, strings.Contains(result.Error(), "/var/"),
					"sanitized error should not contain absolute paths")
				assert.False(t, strings.Contains(result.Error(), "/deep/"),
					"sanitized error should not contain absolute paths")
			}
		})
	}
}

func TestSanitizeErrorMessage_NoAbsolutePathLeak(t *testing.T) {
	paths := []string{
		"/var/lib/operator/data/rules.data",
		"/etc/coraza/secrets/api-keys.conf",
		"/tmp/waf-runtime/cache/modsec.data",
		"/home/operator/.config/rules.txt",
	}
	for _, p := range paths {
		err := &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		result := SanitizeErrorMessage(err).Error()
		assert.False(t, strings.Contains(result, "/"),
			"sanitized output for path %q should not contain any '/' but got: %s", p, result)
	}
}

func TestShouldSkipMissingFileError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		secretData map[string][]byte
		want       bool
	}{
		{
			name:       "nil secretData skips a relative missing file (defer to aggregate)",
			err:        &fs.PathError{Op: "open", Path: "rule.data", Err: fs.ErrNotExist},
			secretData: nil,
			want:       true,
		},
		{
			name:       "basename found in secretData",
			err:        &fs.PathError{Op: "open", Path: "rule.data", Err: fs.ErrNotExist},
			secretData: map[string][]byte{"rule.data": []byte("content")},
			want:       true,
		},
		{
			name: "wrapped PathError, basename found",
			err: fmt.Errorf("outer: %w",
				&fs.PathError{Op: "open", Path: "found.data", Err: fs.ErrNotExist}),
			secretData: map[string][]byte{"found.data": []byte("x")},
			want:       true,
		},
		{
			name:       "basename not in secretData",
			err:        &fs.PathError{Op: "open", Path: "missing.data", Err: fs.ErrNotExist},
			secretData: map[string][]byte{"other.data": []byte("content")},
			want:       false,
		},
		{
			name:       "non-PathError returns false",
			err:        errors.New("generic error"),
			secretData: map[string][]byte{"anything": []byte("x")},
			want:       false,
		},
		{
			name:       "ErrNotExist without PathError returns false",
			err:        fmt.Errorf("wrapped: %w", fs.ErrNotExist),
			secretData: map[string][]byte{"anything": []byte("x")},
			want:       false,
		},
		{
			name:       "PathError with ErrPermission returns false",
			err:        &fs.PathError{Op: "open", Path: "file.data", Err: fs.ErrPermission},
			secretData: map[string][]byte{"file.data": []byte("x")},
			want:       false,
		},
		{
			name:       "embedded CRS include is skipped with an empty data file map",
			err:        &fs.PathError{Op: "open", Path: "crs/crs-setup.conf", Err: fs.ErrNotExist},
			secretData: map[string][]byte{},
			want:       true,
		},
		{
			name:       "legacy alias is not runtime skipped with an empty data file map",
			err:        &fs.PathError{Op: "open", Path: "@recommended-conf", Err: fs.ErrNotExist},
			secretData: map[string][]byte{},
			want:       false,
		},
		{
			name:       "Coraza %s-wrapped missing include is deferred with nil dataFiles",
			err:        errors.New("invalid WAF config from string: failed to readfile: open blocklist.conf: file does not exist"),
			secretData: nil,
			want:       true,
		},
		{
			name:       "Coraza %s-wrapped missing include found in dataFiles",
			err:        errors.New("invalid WAF config from string: failed to readfile: open blocklist.conf: file does not exist"),
			secretData: map[string][]byte{"blocklist.conf": []byte("x")},
			want:       true,
		},
		{
			name:       "Coraza %s-wrapped missing include absent from non-nil dataFiles",
			err:        errors.New("invalid WAF config from string: failed to readfile: open nofile.conf: file does not exist"),
			secretData: map[string][]byte{"blocklist.conf": []byte("x")},
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldSkipMissingFileError(tt.err, tt.secretData)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestValidateRuleSourceRules(t *testing.T) {
	t.Run("valid rules return nil", func(t *testing.T) {
		err := ValidateRuleSourceRules(`SecDefaultAction "phase:1,log,auditlog,pass"`, "test-rs", nil)
		assert.NoError(t, err)
	})

	t.Run("invalid rules return error mentioning RuleSource name", func(t *testing.T) {
		err := ValidateRuleSourceRules(`SecInvalidDirective "bad"`, "bad-rs", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad-rs")
		assert.Contains(t, err.Error(), "doesn't contain valid rules")
	})

	t.Run("missing file error is skipped when file exists in dataFiles", func(t *testing.T) {
		dataFiles := map[string][]byte{"rule1.data": []byte("content")}
		err := ValidateRuleSourceRules(
			`SecRule REQUEST_URI "@pmFromFile rule1.data" "id:1,phase:1,deny"`,
			"data-rs", dataFiles,
		)
		assert.NoError(t, err)
	})

	t.Run("missing file error is reported when file not in non-nil dataFiles", func(t *testing.T) {
		err := ValidateRuleSourceRules(
			`SecRule REQUEST_URI "@pmFromFile missing.data" "id:1,phase:1,deny"`,
			"data-rs", map[string][]byte{},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "data-rs")
		msg := err.Error()
		if strings.Contains(msg, "/") {
			for _, leak := range []string{"/var/", "/etc/", "/tmp/", "/app/", "/root/"} {
				assert.NotContains(t, msg, leak, "validation error leaked a filesystem path segment")
			}
		}
	})

	t.Run("nil dataFiles skips missing file for operator validation path", func(t *testing.T) {
		err := ValidateRuleSourceRules(
			`SecRule REQUEST_URI "@pmFromFile only-at-ruleset.data" "id:1,phase:1,deny"`,
			"defer-rs", nil,
		)
		assert.NoError(t, err)
	})
}

// TestValidateRuleSourceRulesDefersIncludedRuleData covers `Include <RuleData
// name>` on the per-source path: the file is resolved from the RuleSet's merged
// RuleData, so a missing file is deferred to aggregate validation instead of
// degrading the RuleSource. Coraza wraps the read error with %s, which loses the
// *fs.PathError the skip used to require.
func TestValidateRuleSourceRulesDefersIncludedRuleData(t *testing.T) {
	rules := "Include blocklist.conf"

	assert.NoError(t, ValidateRuleSourceRules(rules, "include-rs", nil),
		"a bare RuleData include must be deferred, not rejected")

	dataFiles := map[string][]byte{
		"blocklist.conf": []byte(`SecRule ARGS "@streq evilmonkey" "id:1,phase:1,deny"`),
	}
	assert.NoError(t, ValidateRuleSourceRules(rules, "include-rs", dataFiles))

	err := ValidateRuleSourceRules("Include nope.conf", "include-rs", map[string][]byte{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open nope.conf: data does not exist")
	assert.NotContains(t, err.Error(), "/", "the redacted message must not leak a filesystem path")
}

// TestBuildWAFConfigStubsCRSInIncludedRuleData covers an Include of a RuleData
// file that itself includes embedded CRS data: Coraza parses the included file
// as rule text, so the stubs must be collected from it too.
func TestBuildWAFConfigStubsCRSInIncludedRuleData(t *testing.T) {
	useCRSModule(t)
	dataFiles := map[string][]byte{
		"extra.conf": []byte("Include crs/crs-setup.conf\n" +
			`SecRule ARGS "@pmFromFile crs/restricted-files.data" "id:1,phase:1,pass"` + "\n"),
	}
	rules := "SecRuleEngine On\nInclude extra.conf"

	require.NoError(t, ValidateIncludes(rules, dataFiles))
	require.NoError(t, ValidateRuleSourceRules(rules, "include-rs", dataFiles))
	_, err := coraza.NewWAF(BuildWAFConfig(rules, dataFiles))
	require.NoError(t, err)
}

// TestValidateRuleSourceRulesCannotReadHostFiles covers the tenant-text escape
// where Coraza's default io.OSFS read an operator file and its content leaked
// through the compilation error into tenant-readable status.
func TestValidateRuleSourceRulesCannotReadHostFiles(t *testing.T) {
	const marker = "operator-secret-marker-4f2c"
	secret := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(secret, []byte(marker+"\n"), 0o600))

	for _, include := range []string{
		"Include " + secret,
		"Include /var/run/secrets/kubernetes.io/serviceaccount/token",
		"Include /proc/self/environ",
		"Include ../etc/passwd",
		`Include C:\Windows\win.ini`,
	} {
		t.Run(include, func(t *testing.T) {
			err := ValidateRuleSourceRules(include, "probe-rs", nil)
			require.Error(t, err, "an OS path must be rejected, not silently skipped")
			assert.Contains(t, err.Error(), "probe-rs")
			assert.NotContains(t, err.Error(), marker, "the error must not contain file content")
		})
	}

	t.Run("/proc/self/environ content is not disclosed", func(t *testing.T) {
		t.Setenv("CKO_VALIDATION_PROBE", marker)
		err := ValidateRuleSourceRules("Include /proc/self/environ", "environ-rs", nil)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), marker)
	})

	// The same regression directly on the validation WAF: with an empty
	// in-memory root, no directive may read an arbitrary OS file.
	t.Run("validation WAF cannot read an existing OS file", func(t *testing.T) {
		for _, rules := range []string{
			"Include " + secret,
			`SecRule ARGS "@pmFromFile ` + secret + `" "id:1,phase:1,pass"`,
		} {
			_, err := coraza.NewWAF(BuildWAFConfig(rules, nil))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), marker)
		}
	})

	t.Run("audit log configuration does not touch the host filesystem", func(t *testing.T) {
		auditPath := filepath.Join(t.TempDir(), "audit.log")
		err := ValidateRuleSourceRules(
			"SecRuleEngine On\nSecAuditEngine On\nSecAuditLog "+auditPath+"\nSecAuditLogType Serial",
			"audit-rs", nil,
		)
		require.Error(t, err, "a file audit log target cannot be delivered to the plugin")
		assert.NotContains(t, err.Error(), auditPath, "the error must not echo the tenant path")
		_, statErr := os.Stat(auditPath)
		assert.True(t, os.IsNotExist(statErr), "the validation WAF must not create the audit log on the host")

		require.Error(t, ValidateRuleSourceRules(
			"SecRuleEngine On\nSecAuditEngine On\nSecAuditLog /dev/stdout\nSecAuditLogType Serial",
			"audit-rs", nil,
		), "envoy-coraza has no audit sink")
	})
}

func TestValidateIncludes(t *testing.T) {
	for _, rules := range []string{
		"Include /etc/passwd",
		"Include /var/run/secrets/kubernetes.io/serviceaccount/token",
		"Include ../etc/passwd",
		"Include ..",
		"Include SomeData/../../../etc/passwd",
		"Include data/../../etc/passwd",
		`Include SomeData\..\..\..\etc\passwd`,
		`Include C:\Windows\win.ini`,
		"Include /etc/passwd /etc/shadow",
		"Include \\\n/etc/passwd",
		"SecRuleEngine On\ninclude '/etc/passwd'",
	} {
		t.Run(rules, func(t *testing.T) {
			require.Error(t, ValidateIncludes(rules, nil), "an OS path include must be rejected")
		})
	}

	for _, rules := range []string{
		"Include crs/crs-setup.conf",
		"Include crs/rules/*.conf",
		"Include crs/rules/REQUEST-901-INITIALIZATION.conf",
		"Include some.data",
		"Include ./some.data",
		"SecRuleEngine On\nSecRule ARGS \"@rx x\" \"id:1,phase:1,pass\"",
		"SecDebugLogLevel 9",
		"SecAuditLogType Serial",
	} {
		t.Run(rules, func(t *testing.T) {
			// The crs/ forms need a module built with embed-crs; the rest
			// never depends on the capability.
			if strings.Contains(rules, "crs/") {
				useCRSModule(t)
			}
			assert.NoError(t, ValidateIncludes(rules, nil))
		})
	}
}

// TestValidateIncludesQuotesMirrorCoraza covers the quoting of the Include
// argument. Coraza trims the line, cuts it at the first single space and strips
// every leading and trailing double quote when the whole argument is quoted and
// at least three bytes long, never a single quote (internal/seclang/parser.go,
// evaluateLine). The guard must resolve the target the same way: any other
// quoting names a different file in the plugin, which then builds a zero-rule
// WAF or NACKs.
func TestValidateIncludesQuotesMirrorCoraza(t *testing.T) {
	useCRSModule(t) // the accepted crs/ forms need an embedded CRS
	const marker = "tenant-quote-marker-3e81"
	for _, rules := range []string{
		"Include 'blocklist.conf'",
		"Include blocklist.conf'",
		"SecRuleEngine On\nInclude \"blocklist.conf",
		"Include ''blocklist.conf''",
		"Include \"" + marker + ".conf\"'",
		"Include \"\"",
	} {
		t.Run(rules, func(t *testing.T) {
			err := ValidateIncludes(rules, map[string][]byte{})
			require.Error(t, err, "a quote Coraza does not strip must not pass the guard")
			assert.Contains(t, err.Error(), "quote")
			assert.NotContains(t, err.Error(), marker, "the error must stay static")
		})
	}

	for _, rules := range []string{
		"Include \"crs/crs-setup.conf\"",
		"Include crs/rules/*.conf",
		"Include \"blocklist.conf\"",
		"Include blocklist.conf",
	} {
		t.Run(rules, func(t *testing.T) {
			assert.NoError(t, ValidateIncludes(rules, map[string][]byte{}),
				"Coraza strips the surrounding double quotes, so the target resolves")
		})
	}
}

// TestValidateIncludesRejectsPaddedGlobs covers the one target Coraza does not
// trim before using it: FromFile decides glob vs literal on the raw argument and
// only trims the name it opens afterwards (internal/seclang/parser.go). A padded
// glob therefore matches nothing, Coraza only logs the empty result, and the WAF
// compiles with zero rules while the Engine reports Ready. A padded non-glob is
// trimmed and resolves normally.
func TestValidateIncludesRejectsPaddedGlobs(t *testing.T) {
	useCRSModule(t) // the crs/ glob forms need an embedded CRS
	for _, rules := range []string{
		"Include  crs/rules/*.conf",
		"Include \"crs/rules/*.conf \"",
		"Include \u00a0crs/rules/*.conf",
		"Include \tcrs/rules/*.conf",
	} {
		t.Run(rules, func(t *testing.T) {
			err := ValidateIncludes(rules, map[string][]byte{})
			require.Error(t, err, "Coraza expands the padded glob and matches nothing")
			assert.Contains(t, err.Error(), "glob")
			assert.Contains(t, err.Error(), "crs/rules/*.conf", "the error must name the allowed form")

			err = ValidateRuleSourceRules(rules, "padded-glob-rs", map[string][]byte{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "padded-glob-rs")
		})
	}

	dataFiles := map[string][]byte{"blocklist.data": []byte("SecRuleEngine On\n")}
	for _, rules := range []string{
		"Include crs/rules/*.conf",
		"Include \"crs/rules/*.conf\"",
		"Include  blocklist.data",
		"Include \"blocklist.data \"",
	} {
		t.Run(rules, func(t *testing.T) {
			assert.NoError(t, ValidateIncludes(rules, dataFiles))
			assert.NoError(t, ValidateRuleSourceRules(rules, "padded-rs", dataFiles))
		})
	}

	// Un-guarded, the padded glob is exactly what the Engine reported Ready on:
	// Coraza logs the empty result and compiles a zero-rule WAF without an error.
	waf, err := coraza.NewWAF(BuildWAFConfig("Include  crs/rules/*.conf", nil))
	require.NoError(t, err)
	CloseWAF(waf)
}

// TestValidateIncludesTrimsTargetLikeCoraza covers the whitespace Coraza trims
// around a non-glob Include argument: the guard must resolve the same name Coraza
// opens. Otherwise a padded name is never scanned, while Coraza still reads the
// real file and a SecDebugLog inside it creates a host file during validation.
// A glob is not trimmed (see TestValidateIncludesRejectsPaddedGlobs).
func TestValidateIncludesTrimsTargetLikeCoraza(t *testing.T) {
	const marker = "padding-marker-7b12"
	debugLog := filepath.Join(t.TempDir(), marker+".log")
	dataFiles := map[string][]byte{
		"blocklist.data": []byte("SecDebugLog " + debugLog + "\n"),
	}

	for _, rules := range []string{
		"Include \u00a0blocklist.data",
		"Include \"blocklist.data \"",
	} {
		t.Run(rules, func(t *testing.T) {
			err := ValidateIncludes(rules, dataFiles)
			require.Error(t, err, "Coraza trims the argument, so the file must be scanned")
			assert.Contains(t, err.Error(), "blocklist.data")
			assert.Contains(t, err.Error(), "SecDebugLog is not allowed")
			assert.NotContains(t, err.Error(), marker, "the error must stay static")

			err = ValidateRuleSourceRules(rules, "padding-rs", dataFiles)
			require.Error(t, err)
			_, statErr := os.Stat(debugLog)
			assert.True(t, os.IsNotExist(statErr), "rejected rules must not create the debug log on the host")
		})
	}

	assert.NoError(t, ValidateIncludes("Include blocklist.data", map[string][]byte{
		"blocklist.data": []byte("word\nother-word\n"),
	}), "a clean RuleData file is still accepted")
}

// TestValidateIncludesRejectsUnresolvableGlobs covers globs the plugin does not
// support. Only crs/rules/*.conf is accepted; all other patterns are rejected
// rather than risking an empty or malformed rule set.
func TestValidateIncludesRejectsUnresolvableGlobs(t *testing.T) {
	for _, target := range []string{
		"crs/rules/*",
		"crs/rules/*.data",
		"crs/rules/REQUEST-*.conf",
		"crs/rules/NOSUCH-*.conf",
		"crs/rules/*[.conf",
		"crs/rules/sub/*.conf",
		"crs/rules/*/*.conf",
		"crs/*.conf",
		"@owasp_crs/*.conf",
		"@recommended-conf*",
		"*.conf",
		"*.data",
		"rules/*.conf",
		"./*.conf",
	} {
		t.Run(target, func(t *testing.T) {
			err := ValidateIncludes("Include "+target, nil)
			require.Error(t, err, "the plugin does not support this glob")
			assert.Contains(t, err.Error(), "crs/rules/*.conf", "the error must name the supported CRS form")
		})
	}
}

// TestValidateIncludesRejectsNonRuleCrsFiles covers literal CRS-tree paths that
// name a file which is not rule text: the .conf.example templates hold no active
// rules (a zero-rule WAF) and the .data files fail to parse as SecLang
// ("unknown directive", so the plugin NACKs the push). Coraza reports neither as
// a rule-loading problem, so the Engine was Ready while the plugin rejected the
// push or enforced nothing.
func TestValidateIncludesRejectsNonRuleCrsFiles(t *testing.T) {
	useCRSModule(t) // so only the non-rule file form, not the capability, rejects
	for _, target := range []string{
		"crs/scanners-user-agents.data",
		"crs/REQUEST-900-EXCLUSION-RULES-BEFORE-CRS.conf.example",
		// The plugin cleans the path, so a leading ./ reaches the same file.
		"./crs/scanners-user-agents.data",
		// The crs directory itself is not a file either.
		"crs",
		"crs/",
	} {
		t.Run(target, func(t *testing.T) {
			for _, dataFiles := range []map[string][]byte{nil, {}} {
				err := ValidateIncludes("Include "+target, dataFiles)
				require.Error(t, err, "only .conf rule files may be included")
				assert.Contains(t, err.Error(), "crs/rules/*.conf")
				assert.Contains(t, err.Error(), "crs/rules/<file>.conf")
				assert.NotContains(t, err.Error(), "conf.example", "the error must not echo the tenant's path")
			}
		})
	}

	// The setup file, rule glob, literal rule file and a bare RuleData name stay accepted.
	assert.NoError(t, ValidateIncludes("Include crs/crs-setup.conf", nil))
	assert.NoError(t, ValidateIncludes("Include crs/rules/*.conf", nil))
	assert.NoError(t, ValidateIncludes("Include crs/rules/REQUEST-901-INITIALIZATION.conf", nil))
	assert.NoError(t, ValidateIncludes("Include blocklist.data", nil), "a bare RuleData name is deferred")
}

// TestValidateRuleSourceRulesRejectsNonRuleCrsFile covers the RuleSource entry
// point, which must reject the same literal before the Engine looks Ready.
func TestValidateRuleSourceRulesRejectsNonRuleCrsFile(t *testing.T) {
	err := ValidateRuleSourceRules("Include crs/restricted-files.data", "crs-rs", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "crs-rs")
	assert.Contains(t, err.Error(), "crs/rules/<file>.conf")
}

// TestValidateIncludesNestedIncludeResolvesRelativeToIncludingFile covers a
// nested Include: Coraza joins the target to the including file's directory,
// so the guard must scan that file, not a same-named file at the root.
func TestValidateIncludesNestedIncludeResolvesRelativeToIncludingFile(t *testing.T) {
	dataFiles := map[string][]byte{
		"sub/a.data": []byte("Include b.data"),
		"b.data":     []byte("SecDebugLog /dev/stdout"),
		"sub/b.data": []byte("SecRuleEngine On"),
	}

	require.NoError(t, ValidateIncludes("Include sub/a.data", dataFiles),
		"the root-level b.data is not the file Coraza parses")

	dataFiles["sub/b.data"] = []byte("SecDebugLog /dev/stdout")
	err := ValidateIncludes("Include sub/a.data", dataFiles)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "included RuleData file sub/b.data")

	// RuleData names cannot contain "/", so a same-directory nested include is
	// the only shape that reaches the operator.
	sameDir := map[string][]byte{
		"outer.data": []byte("Include inner.data"),
		"inner.data": []byte("SecDebugLog /dev/stdout"),
	}
	err = ValidateIncludes("Include outer.data", sameDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "included RuleData file inner.data")

	sameDir["inner.data"] = []byte("SecRuleEngine On")
	require.NoError(t, ValidateIncludes("Include outer.data", sameDir))
}

// TestValidateIncludesRejectsUnknownAliases covers legacy Go-plugin aliases:
// envoy-coraza does not resolve any @-prefixed Include target.
func TestValidateIncludesRejectsUnknownAliases(t *testing.T) {
	for _, target := range []string{
		"@notanalias",
		"@",
		"@crs-setup",
		"@OWASP_CRS/REQUEST-901-INITIALIZATION.conf",
		"@owasp_crs/*.conf",
		"@owasp_crs/../setup.conf",
		"@recommended-conf-extra",
		"@recommended-conf",
	} {
		t.Run(target, func(t *testing.T) {
			err := ValidateIncludes("Include "+target, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "crs/crs-setup.conf")
			assert.Contains(t, err.Error(), "RuleData file name")
			assert.NotContains(t, err.Error(), target, "the error must stay static")
		})
	}
}

// TestValidateIncludesRejectsHostWritingDirectives covers the directives that
// make Coraza create or append host files while compiling a WAF. The error must
// be static: tenant-controlled paths and rule content stay out of status.
func TestValidateIncludesRejectsHostWritingDirectives(t *testing.T) {
	const marker = "tenant-path-marker-7b1d"
	for _, rules := range []string{
		"SecDebugLog /dev/stdout",
		"SecDebugLog /dev/shm/" + marker + ".log",
		"SecDebugLogLevel 9\nSecDebugLog /dev/stdout",
		"secdebuglog /dev/stdout",
		"SecUploadDir /dev/shm/" + marker,
		"SecUploadKeepFiles On",
		"SecUploadKeepFiles",
		"SecUploadKeepFiles \"On\"",
		"secuploadkeepfiles on",
	} {
		t.Run(rules, func(t *testing.T) {
			err := ValidateIncludes(rules, nil)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), marker)
			assert.NotContains(t, err.Error(), "/dev/")
			assert.NotContains(t, err.Error(), "shm")
		})
	}

	assert.NoError(t, ValidateIncludes("SecDebugLogLevel 9", nil))
	assert.NoError(t, ValidateIncludes("SecDebugLog2 /dev/stdout", nil))
	// "Off" has no host effect and appears in stock CRS snippets.
	assert.NoError(t, ValidateIncludes("SecUploadKeepFiles Off", nil))
	assert.NoError(t, ValidateIncludes("SecUploadKeepFiles \"Off\"", nil))
}

// TestValidateIncludesRejectsAuditLogFileTargets covers the delivered audit log
// path: envoy-coraza has no audit sink, so every SecAuditLog is rejected.
func TestValidateIncludesRejectsAuditLogFileTargets(t *testing.T) {
	const marker = "tenant-audit-marker-9ad2"
	for _, rules := range []string{
		"SecAuditLog /var/log/" + marker + ".log",
		"SecAuditLog \"/var/log/" + marker + ".log\"",
		"SecAuditLog /dev/null",
		"SecAuditLog",
		"SecAuditLog ",
		"SecAuditLog  /dev/stdout",
		"secauditlog audit-" + marker + ".log",
		"SecAuditLogType Serial\nSecAuditLog /var/log/" + marker + ".log",
	} {
		t.Run(rules, func(t *testing.T) {
			err := ValidateIncludes(rules, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "SecAuditLog is not supported by envoy-coraza")
			assert.NotContains(t, err.Error(), marker, "the error must be content free")
			assert.NotContains(t, err.Error(), "/var/")
		})
	}

	// SecAuditLogType alone is still accepted when it is the supported type.
	assert.NoError(t, ValidateIncludes("SecAuditLogType Serial", nil))
	assert.NoError(t, ValidateIncludes("secauditlogtype serial", nil))
	assert.NoError(t, ValidateIncludes("SecAuditLogType \"Serial\"", nil))
}

// TestValidateIncludesRejectsUnsupportedAuditLogType covers the only audit type
// envoy-coraza can compile: it has no Concurrent, Syslog, or Https sink.
func TestValidateIncludesRejectsUnsupportedAuditLogType(t *testing.T) {
	for _, rules := range []string{
		"SecAuditLogType Concurrent",
		"SecAuditLogType Syslog",
		"SecAuditLogType Https",
		"SecAuditLogType Serial\nSecAuditLogType Concurrent",
		"SecAuditLogType",
	} {
		t.Run(rules, func(t *testing.T) {
			err := ValidateIncludes(rules, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "only supported as Serial")
			assert.NotContains(t, err.Error(), "/var/")
		})
	}
}

// TestValidateIncludesRejectsRequestBodyLimitOverWasmCap covers the limit
// envoy-coraza refuses: its WASM host caps the request body at 16 MiB.
func TestValidateIncludesRejectsRequestBodyLimitOverWasmCap(t *testing.T) {
	for _, rules := range []string{
		"SecRequestBodyAccess On\nSecRequestBodyLimit 16777217",
		"SecRequestBodyLimit 16777217",
		"SecRequestBodyLimit 0",
		"SecRequestBodyLimit -1",
		"SecRequestBodyLimit not-a-number",
	} {
		t.Run(rules, func(t *testing.T) {
			err := ValidateIncludes(rules, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "SecRequestBodyLimit")
		})
	}

	for _, rules := range []string{
		"SecRequestBodyAccess On\nSecRequestBodyLimit 13107200",
		"SecRequestBodyAccess On\nSecRequestBodyLimit 16777216",
		"SecRequestBodyAccess On\nSecRequestBodyLimit \"16777216\"",
		"SecResponseBodyAccess Off",
	} {
		t.Run(rules, func(t *testing.T) {
			assert.NoError(t, ValidateIncludes(rules, nil))
		})
	}
}

func TestValidateRuleSourceRulesAcceptsEmbeddedCRS(t *testing.T) {
	useCRSModule(t)
	for _, include := range []string{
		"Include crs/crs-setup.conf",
		"Include crs/rules/*.conf",
		"Include crs/rules/REQUEST-901-INITIALIZATION.conf",
		"SecRuleEngine On\nInclude crs/crs-setup.conf\nInclude crs/rules/*.conf",
		`SecRule ARGS "@pmFromFile crs/restricted-files.data" "id:1,phase:1,pass"`,
	} {
		t.Run(include, func(t *testing.T) {
			assert.NoError(t, ValidateRuleSourceRules(include, "crs-rs", nil), "the plugin resolves this CRS reference at runtime")
			assert.NoError(t, ValidateRuleSourceRules(include, "crs-rs", map[string][]byte{}))
		})
	}
}

// TestValidateRuleSourceRulesRejectsUnknownAlias covers the RuleSource entry
// point: legacy Go-plugin Include aliases must fail before the Engine looks Ready.
func TestValidateRuleSourceRulesRejectsUnknownAlias(t *testing.T) {
	for _, include := range []string{"Include @owasp_crs/*.conf", "Include @recommended-conf"} {
		err := ValidateRuleSourceRules(include, "alias-rs", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "alias-rs")
		assert.Contains(t, err.Error(), "crs/crs-setup.conf")
		assert.Contains(t, err.Error(), "RuleData file name")
		assert.NotContains(t, err.Error(), "@owasp_crs")
	}
}

// TestValidateRuleSourceRulesScansPatchOnlyFragments covers patch-only
// fragments, which skip Coraza but still carry Include directives the plugin
// must resolve.
func TestValidateRuleSourceRulesScansPatchOnlyFragments(t *testing.T) {
	rules := "SecRuleRemoveById 1\nInclude @notanalias"
	assert.True(t, IsPatchOnlyFragment(rules), "test requires a patch-only fragment")
	require.NoError(t, ValidateRuleSourceRules("SecRuleRemoveById 1", "patch-rs", nil))

	err := ValidateRuleSourceRules(rules, "patch-rs", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "patch-rs")
	assert.Contains(t, err.Error(), "crs/crs-setup.conf")
}

// TestValidateRuleSourceRulesRejectsDebugLog covers the RuleSource entry point:
// SecDebugLog must not reach NewWAF, which opens the path on the host.
func TestValidateRuleSourceRulesRejectsDebugLog(t *testing.T) {
	const marker = "probe-debug-marker-56af"
	debugLog := filepath.Join(t.TempDir(), marker+".log")

	err := ValidateRuleSourceRules(
		"SecRuleEngine On\nSecDebugLogLevel 9\nSecDebugLog "+debugLog,
		"debug-rs", nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "debug-rs")
	assert.NotContains(t, err.Error(), marker, "the error must not echo the tenant path")

	_, statErr := os.Stat(debugLog)
	assert.True(t, os.IsNotExist(statErr), "rejected rules must not create the debug log on the host")
}

// TestValidateIncludesScansIncludedDataFiles covers a RuleData file that the
// rule text Includes: Coraza parses an included file as rule text, so a
// host-writing directive hidden there must be rejected too. A @pmFromFile data
// file is never parsed as SecLang, so directive text inside it is just data.
func TestValidateIncludesScansIncludedDataFiles(t *testing.T) {
	const marker = "data-content-marker-2c94"
	dataFiles := map[string][]byte{
		"bad.data": []byte("SecDebugLog /dev/shm/" + marker + "\n"),
	}

	err := ValidateIncludes("Include bad.data", dataFiles)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad.data")
	assert.NotContains(t, err.Error(), marker)

	err = ValidateRuleSourceRules("Include bad.data", "data-rs", dataFiles)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "data-rs")
	assert.NotContains(t, err.Error(), marker)

	assert.NoError(t, ValidateIncludes("Include good.data", map[string][]byte{
		"good.data": []byte("word\nother-word\n"),
	}))

	// A blocklist that contains the text of a forbidden directive is a valid
	// pattern file: @pmFromFile data is not SecLang.
	pmfRules := `SecRule ARGS "@pmf blocklist.data" "id:1,phase:1,deny"`
	blocklist := map[string][]byte{
		"blocklist.data": []byte("evilmonkey\nSecUploadDir /tmp\nSecDebugLog /dev/stdout\n"),
	}
	assert.NoError(t, ValidateIncludes(pmfRules, blocklist))
	assert.NoError(t, ValidateRuleSourceRules(pmfRules, "pmf-rs", blocklist))

	// An included file that Includes another RuleData file is parsed too, so its
	// content is reached by the scan.
	nested := map[string][]byte{
		"outer.data": []byte("Include inner.data\n"),
		"inner.data": []byte("SecDebugLog /dev/shm/" + marker + "\n"),
	}
	err = ValidateIncludes("Include outer.data", nested)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inner.data")
	assert.NotContains(t, err.Error(), marker)
}

// TestBuildWAFConfigSecDebugLogWritesHostFile documents why SecDebugLog is
// rejected: the validation WAF itself opens the tenant path on the host.
func TestBuildWAFConfigSecDebugLogWritesHostFile(t *testing.T) {
	debugLog := filepath.Join(t.TempDir(), "debug.log")
	_, err := coraza.NewWAF(BuildWAFConfig("SecDebugLog "+debugLog+"\nSecDebugLogLevel 9", nil))
	require.NoError(t, err)

	_, statErr := os.Stat(debugLog)
	assert.NoError(t, statErr, "without the validation guard Coraza creates the debug log on the host")
}

// TestBuildWAFConfigTrailerIsNotGlued covers rule text that ends in a line
// continuation: Coraza skips blank lines before joining a continued line, so
// the trailer's first directive would be swallowed into the last rule and
// compilation would fail with a confusing action error. The guard rejects it.
func TestBuildWAFConfigTrailerIsNotGlued(t *testing.T) {
	rules := "SecRuleEngine On\nSecRule ARGS \"@rx x\" \"id:1,phase:1,pass\"\\\n"

	for _, check := range []func(string, string, map[string][]byte) error{
		func(rules, _ string, dataFiles map[string][]byte) error { return ValidateIncludes(rules, dataFiles) },
		ValidateRuleSourceRules,
	} {
		err := check(rules, "continuation-rs", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "line continuation")
		assert.NotContains(t, err.Error(), "SecAuditLogType")
	}

	// The same rules with the trailing continuation removed compile normally.
	_, err := coraza.NewWAF(BuildWAFConfig(strings.TrimSuffix(rules, "\\\n"), nil))
	require.NoError(t, err)
}

// TestBuildWAFConfigStubsEmbeddedCRSData covers the embedded CRS data a tenant
// references from its own rules: the plugin serves crs/... from its module, so
// the validation WAF must not fail on it.
func TestBuildWAFConfigStubsEmbeddedCRSData(t *testing.T) {
	_, err := coraza.NewWAF(BuildWAFConfig(
		"SecRule ARGS \"@pmf crs/restricted-files.data\" \"id:1,phase:1,pass\"", nil))
	require.NoError(t, err)
}

// TestValidationWAFFDsAreNotLeaked covers the audit writer the trailer pins:
// Coraza opens the SecAuditLog target at NewWAF and nothing closes a file
// target, so a file trailer such as /dev/null would add one descriptor per
// validation, and tenants can trigger a validation per RuleSource change.
// Linux only: the count comes from /proc/self/fd.
func TestValidationWAFFDsAreNotLeaked(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("open file descriptors are counted from /proc/self/fd")
	}
	openFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		require.NoError(t, err)
		return len(entries)
	}
	build := func() {
		waf, err := coraza.NewWAF(BuildWAFConfig("SecRuleEngine On", nil))
		require.NoError(t, err)
		CloseWAF(waf)
	}

	build() // settle any descriptor opened lazily by the first build
	before := openFDs()
	for range 50 {
		build()
	}
	// Fewer is fine (the runtime may close a lazily opened file); a leak shows
	// up as one descriptor per build.
	assert.LessOrEqual(t, openFDs(), before, "validation must not leak file descriptors")
}
