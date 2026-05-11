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
	"strings"
	"syscall"
	"testing"

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
			name:       "nil secretData always returns false",
			err:        &fs.PathError{Op: "open", Path: "/path/to/rule.data", Err: fs.ErrNotExist},
			secretData: nil,
			want:       false,
		},
		{
			name:       "basename found in secretData",
			err:        &fs.PathError{Op: "open", Path: "/some/dir/rule.data", Err: fs.ErrNotExist},
			secretData: map[string][]byte{"rule.data": []byte("content")},
			want:       true,
		},
		{
			name: "wrapped PathError, basename found",
			err: fmt.Errorf("outer: %w",
				&fs.PathError{Op: "open", Path: "/x/y/z/found.data", Err: fs.ErrNotExist}),
			secretData: map[string][]byte{"found.data": []byte("x")},
			want:       true,
		},
		{
			name:       "basename not in secretData",
			err:        &fs.PathError{Op: "open", Path: "/path/to/missing.data", Err: fs.ErrNotExist},
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
			err:        &fs.PathError{Op: "open", Path: "/path/to/file.data", Err: fs.ErrPermission},
			secretData: map[string][]byte{"file.data": []byte("x")},
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

	t.Run("missing file error is reported when file not in dataFiles", func(t *testing.T) {
		err := ValidateRuleSourceRules(
			`SecRule REQUEST_URI "@pmFromFile missing.data" "id:1,phase:1,deny"`,
			"data-rs", nil,
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
}
