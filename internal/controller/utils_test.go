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

package controller

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
)

func TestCacheEntryPayloadSize(t *testing.T) {
	tests := []struct {
		name      string
		rules     string
		dataFiles map[string][]byte
		expected  int
	}{
		{
			name:      "empty rules, nil data",
			rules:     "",
			dataFiles: nil,
			expected:  0,
		},
		{
			name:      "rules only",
			rules:     "SecRule REQUEST_URI",
			dataFiles: nil,
			expected:  len("SecRule REQUEST_URI"),
		},
		{
			name:  "rules plus data files",
			rules: "abc",
			dataFiles: map[string][]byte{
				"file.data": []byte("content"),
			},
			expected: len("abc") + len("file.data") + len("content"),
		},
		{
			name:  "multiple data files",
			rules: "",
			dataFiles: map[string][]byte{
				"a.data": []byte("x"),
				"b.data": []byte("yy"),
			},
			expected: len("a.data") + 1 + len("b.data") + 2,
		},
		{
			name:      "empty data map",
			rules:     "r",
			dataFiles: map[string][]byte{},
			expected:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cacheEntryPayloadSize(tt.rules, tt.dataFiles)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestCacheEntryPayloadSize_matchesRuleSetCacheTotalSize(t *testing.T) {
	c := cache.NewRuleSetCache()
	rules := "SecRule"
	data := map[string][]byte{"rule1.data": []byte("payload")}
	want := cacheEntryPayloadSize(rules, data)
	c.Put("test/instance", rules, data)
	assert.Equal(t, want, c.TotalSize(), "payload accounting must match RuleSetCache.TotalSize for a single stored entry")
}

func TestOversizedAdmissionUsesStrictGreaterThanBudget(t *testing.T) {
	budget := 1000
	rules := strings.Repeat("a", 1000)
	assert.Equal(t, 1000, cacheEntryPayloadSize(rules, nil))
	assert.False(t, cacheEntryPayloadSize(rules, nil) > budget, "equal payload must be admitted (check uses > not >=)")
	assert.True(t, cacheEntryPayloadSize(rules+"x", nil) > budget)
}

func TestBuildCacheReadyMessage(t *testing.T) {
	t.Run("without unsupported message", func(t *testing.T) {
		msg := buildCacheReadyMessage("ns", "my-rules", "")
		assert.Equal(t, "Successfully cached rules for ns/my-rules", msg)
	})

	t.Run("with unsupported message", func(t *testing.T) {
		msg := buildCacheReadyMessage("ns", "my-rules", "found unsupported rule 950150")
		assert.Contains(t, msg, "Successfully cached rules for ns/my-rules")
		assert.Contains(t, msg, "[annotation override]")
		assert.Contains(t, msg, "950150")
	})
}

func TestCollectRequests(t *testing.T) {
	engines := []wafv1alpha1.Engine{
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns2"}},
	}

	t.Run("empty slice returns nil", func(t *testing.T) {
		got := collectRequests([]wafv1alpha1.Engine{}, func(e *wafv1alpha1.Engine) bool {
			return true
		})
		assert.Nil(t, got)
	})

	t.Run("no matches returns nil", func(t *testing.T) {
		got := collectRequests(engines, func(e *wafv1alpha1.Engine) bool {
			return false
		})
		assert.Nil(t, got)
	})

	t.Run("all match", func(t *testing.T) {
		got := collectRequests(engines, func(e *wafv1alpha1.Engine) bool {
			return true
		})
		assert.Equal(t, []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: "a", Namespace: "ns1"}},
			{NamespacedName: types.NamespacedName{Name: "b", Namespace: "ns1"}},
			{NamespacedName: types.NamespacedName{Name: "c", Namespace: "ns2"}},
		}, got)
	})

	t.Run("partial match", func(t *testing.T) {
		got := collectRequests(engines, func(e *wafv1alpha1.Engine) bool {
			return e.Namespace == "ns1"
		})
		assert.Equal(t, []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: "a", Namespace: "ns1"}},
			{NamespacedName: types.NamespacedName{Name: "b", Namespace: "ns1"}},
		}, got)
	})
}

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
			base, ok := extractMissingFileBasename(tt.err)
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
			result := sanitizeErrorMessage(tt.err)
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
		result := sanitizeErrorMessage(err).Error()
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
			got := shouldSkipMissingFileError(tt.err, tt.secretData)
			require.Equal(t, tt.want, got)
		})
	}
}
