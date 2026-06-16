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

package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authclient "k8s.io/client-go/kubernetes/typed/authentication/v1"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// -----------------------------------------------------------------------------
// Fake TokenReview client for unit tests
// -----------------------------------------------------------------------------

// fakeTokenReview implements authclient.TokenReviewInterface for unit testing.
// It only needs Create since that's the sole method on the interface.
type fakeTokenReview struct {
	// tokens maps token strings to their authentication result.
	tokens map[string]fakeTokenResult
}

type fakeTokenResult struct {
	authenticated bool
	username      string
	audiences     []string
}

var _ authclient.TokenReviewInterface = &fakeTokenReview{}

func (f *fakeTokenReview) Create(_ context.Context, review *authv1.TokenReview, _ metav1.CreateOptions) (*authv1.TokenReview, error) {
	result, ok := f.tokens[review.Spec.Token]
	if !ok {
		return &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{Authenticated: false},
		}, nil
	}

	// Check audience match if audiences are specified in the review request.
	if len(review.Spec.Audiences) > 0 {
		matched := false
		for _, reqAud := range review.Spec.Audiences {
			for _, tokenAud := range result.audiences {
				if reqAud == tokenAud {
					matched = true
					break
				}
			}
		}
		if !matched {
			return &authv1.TokenReview{
				Status: authv1.TokenReviewStatus{Authenticated: false},
			}, nil
		}
	}

	return &authv1.TokenReview{
		Status: authv1.TokenReviewStatus{
			Authenticated: result.authenticated,
			User:          authv1.UserInfo{Username: result.username},
			Audiences:     result.audiences,
		},
	}, nil
}

// newFakeTokenReview creates a fakeTokenReview with preconfigured tokens.
func newFakeTokenReview(tokens map[string]fakeTokenResult) *fakeTokenReview {
	return &fakeTokenReview{tokens: tokens}
}

// newNoopTokenReview creates a fakeTokenReview that rejects all tokens.
// Used by existing tests that don't exercise authentication.
func newNoopTokenReview() *fakeTokenReview {
	return &fakeTokenReview{tokens: map[string]fakeTokenResult{}}
}

// -----------------------------------------------------------------------------
// Tests - Token Authenticator
// -----------------------------------------------------------------------------

func TestTokenAuthenticator_Authenticate(t *testing.T) {
	testAudience := Audience("test-ns/my-ruleset")

	fake := newFakeTokenReview(map[string]fakeTokenResult{
		"valid-token": {
			authenticated: true,
			username:      "system:serviceaccount:test-ns:coraza-engine-my-engine",
			audiences:     []string{testAudience},
		},
		"wrong-audience-token": {
			authenticated: true,
			username:      "system:serviceaccount:test-ns:coraza-engine-my-engine",
			audiences:     []string{"wrong-audience"},
		},
		"not-sa-token": {
			authenticated: true,
			username:      "admin",
			audiences:     []string{testAudience},
		},
	})

	auth := NewTokenAuthenticator(fake)
	ctx := context.Background()

	t.Run("valid token", func(t *testing.T) {
		result, err := auth.Authenticate(ctx, "valid-token", testAudience)
		require.NoError(t, err)
		assert.Equal(t, "test-ns", result.Namespace)
		assert.Equal(t, "coraza-engine-my-engine", result.Name)
	})

	t.Run("unknown token", func(t *testing.T) {
		_, err := auth.Authenticate(ctx, "unknown-token", testAudience)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not authenticated")
	})

	t.Run("wrong audience", func(t *testing.T) {
		_, err := auth.Authenticate(ctx, "wrong-audience-token", testAudience)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not authenticated")
	})

	t.Run("not a service account", func(t *testing.T) {
		_, err := auth.Authenticate(ctx, "not-sa-token", testAudience)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a service account")
	})
}

func TestParseServiceAccountUsername(t *testing.T) {
	tests := []struct {
		username      string
		wantNamespace string
		wantName      string
		wantErr       bool
	}{
		{
			username:      "system:serviceaccount:default:coraza-engine-my-engine",
			wantNamespace: "default",
			wantName:      "coraza-engine-my-engine",
		},
		{
			username:      "system:serviceaccount:kube-system:coraza-engine-test",
			wantNamespace: "kube-system",
			wantName:      "coraza-engine-test",
		},
		{
			username: "admin",
			wantErr:  true,
		},
		{
			username: "system:serviceaccount:",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.username, func(t *testing.T) {
			ns, name, err := parseServiceAccountUsername(tt.username)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantNamespace, ns)
			assert.Equal(t, tt.wantName, name)
		})
	}
}

// -----------------------------------------------------------------------------
// Tests - Auth Cache Behavior
// -----------------------------------------------------------------------------

// countingTokenReview wraps fakeTokenReview and counts API calls.
type countingTokenReview struct {
	inner    *fakeTokenReview
	apiCalls int
}

func (c *countingTokenReview) Create(ctx context.Context, review *authv1.TokenReview, opts metav1.CreateOptions) (*authv1.TokenReview, error) {
	c.apiCalls++
	return c.inner.Create(ctx, review, opts)
}

func TestTokenAuthenticator_CachesSuccessfulResults(t *testing.T) {
	testAudience := Audience("test-ns/my-ruleset")
	inner := newFakeTokenReview(map[string]fakeTokenResult{
		"valid-token": {
			authenticated: true,
			username:      "system:serviceaccount:test-ns:coraza-engine-my-engine",
			audiences:     []string{testAudience},
		},
	})
	counting := &countingTokenReview{inner: inner}
	auth := NewTokenAuthenticator(counting)
	ctx := context.Background()

	// First call should hit the API.
	result1, err := auth.Authenticate(ctx, "valid-token", testAudience)
	require.NoError(t, err)
	assert.Equal(t, "test-ns", result1.Namespace)
	assert.Equal(t, 1, counting.apiCalls)

	// Second call should use cache, not hit API.
	result2, err := auth.Authenticate(ctx, "valid-token", testAudience)
	require.NoError(t, err)
	assert.Equal(t, "test-ns", result2.Namespace)
	assert.Equal(t, 1, counting.apiCalls, "second call should use cache")
}

func TestTokenAuthenticator_DoesNotCacheFailures(t *testing.T) {
	inner := newFakeTokenReview(map[string]fakeTokenResult{})
	counting := &countingTokenReview{inner: inner}
	auth := NewTokenAuthenticator(counting)
	ctx := context.Background()
	testAudience := Audience("test-ns/my-ruleset")

	// First call fails — should hit API.
	_, err := auth.Authenticate(ctx, "bad-token", testAudience)
	require.Error(t, err)
	assert.Equal(t, 1, counting.apiCalls)

	// Second call should also hit API (failures not cached).
	_, err = auth.Authenticate(ctx, "bad-token", testAudience)
	require.Error(t, err)
	assert.Equal(t, 2, counting.apiCalls, "failed auth should not be cached")
}

func TestTokenAuthenticator_CacheExpiry(t *testing.T) {
	testAudience := Audience("test-ns/my-ruleset")
	inner := newFakeTokenReview(map[string]fakeTokenResult{
		"valid-token": {
			authenticated: true,
			username:      "system:serviceaccount:test-ns:coraza-engine-my-engine",
			audiences:     []string{testAudience},
		},
	})
	counting := &countingTokenReview{inner: inner}
	auth := NewTokenAuthenticator(counting)
	// Use a very short TTL so we can test expiry.
	auth.cacheTTL = 10 * time.Millisecond
	ctx := context.Background()

	// First call populates cache.
	_, err := auth.Authenticate(ctx, "valid-token", testAudience)
	require.NoError(t, err)
	assert.Equal(t, 1, counting.apiCalls)

	// Wait for cache to expire.
	time.Sleep(20 * time.Millisecond)

	// Third call should hit API again after expiry.
	_, err = auth.Authenticate(ctx, "valid-token", testAudience)
	require.NoError(t, err)
	assert.Equal(t, 2, counting.apiCalls, "expired cache entry should trigger new API call")
}

func TestTokenAuthenticator_CacheSizeLimit(t *testing.T) {
	testAudience := Audience("ns/my-ruleset")
	// Create many unique valid tokens.
	tokens := make(map[string]fakeTokenResult)
	for i := 0; i < maxAuthCacheSize+100; i++ {
		token := fmt.Sprintf("token-%d", i)
		tokens[token] = fakeTokenResult{
			authenticated: true,
			username:      fmt.Sprintf("system:serviceaccount:ns:coraza-engine-eng-%d", i),
			audiences:     []string{testAudience},
		}
	}
	inner := newFakeTokenReview(tokens)
	auth := NewTokenAuthenticator(inner)
	ctx := context.Background()

	// Authenticate more tokens than the cache can hold.
	for i := 0; i < maxAuthCacheSize+100; i++ {
		token := fmt.Sprintf("token-%d", i)
		_, err := auth.Authenticate(ctx, token, testAudience)
		require.NoError(t, err)
	}

	// Cache size should not exceed the limit.
	auth.cacheMu.Lock()
	size := auth.cacheSize
	auth.cacheMu.Unlock()
	assert.LessOrEqual(t, size, int64(maxAuthCacheSize), "cache size must not exceed limit")
}

// -----------------------------------------------------------------------------
// Tests - Server Authentication Integration
// -----------------------------------------------------------------------------

func TestServer_HandleRules_Authentication(t *testing.T) {
	c := NewRuleSetCache()
	c.Put("test-ns/my-ruleset", "SecRule test", nil)

	rulesetAudience := Audience("test-ns/my-ruleset")

	fake := newFakeTokenReview(map[string]fakeTokenResult{
		"valid-token": {
			authenticated: true,
			username:      "system:serviceaccount:test-ns:coraza-engine-my-engine",
			audiences:     []string{rulesetAudience},
		},
		"wrong-ns-token": {
			authenticated: true,
			username:      "system:serviceaccount:other-ns:coraza-engine-other-engine",
			audiences:     []string{rulesetAudience},
		},
		"wrong-audience-token": {
			authenticated: true,
			username:      "system:serviceaccount:test-ns:coraza-engine-my-engine",
			audiences:     []string{Audience("test-ns/other-ruleset")},
		},
	})

	logger := utils.NewTestLogger(t)
	server := NewServer(c, testServerAddr, logger, nil, fake)

	t.Run("missing authorization header", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rules/test-ns/my-ruleset", nil)
		w := httptest.NewRecorder()
		server.handleRules(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("invalid bearer token", func(t *testing.T) {
		before := testutil.ToFloat64(authFailuresTotal)
		req := httptest.NewRequest(http.MethodGet, "/rules/test-ns/my-ruleset", nil)
		req.Header.Set("Authorization", "Bearer invalid-token")
		w := httptest.NewRecorder()
		server.handleRules(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, before+1, testutil.ToFloat64(authFailuresTotal))
	})

	t.Run("valid token for correct ruleset", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rules/test-ns/my-ruleset", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		w := httptest.NewRecorder()
		server.handleRules(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		var response RuleSetEntry
		err := json.NewDecoder(w.Body).Decode(&response)
		require.NoError(t, err)
		assert.Equal(t, "SecRule test", response.Rules)
	})

	t.Run("token from wrong namespace", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rules/test-ns/my-ruleset", nil)
		req.Header.Set("Authorization", "Bearer wrong-ns-token")
		w := httptest.NewRecorder()
		server.handleRules(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("token with wrong audience", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rules/test-ns/my-ruleset", nil)
		req.Header.Set("Authorization", "Bearer wrong-audience-token")
		w := httptest.NewRecorder()
		server.handleRules(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("valid token for latest endpoint", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rules/test-ns/my-ruleset/latest", nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		w := httptest.NewRecorder()
		server.handleRules(w, req)
		assert.Equal(t, http.StatusOK, w.Code)

		var response LatestResponse
		err := json.NewDecoder(w.Body).Decode(&response)
		require.NoError(t, err)
		assert.NotEmpty(t, response.UUID)
	})

	t.Run("method not allowed still works without auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/rules/test-ns/my-ruleset", nil)
		w := httptest.NewRecorder()
		server.handleRules(w, req)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})

	t.Run("empty path still works without auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rules/", nil)
		w := httptest.NewRecorder()
		server.handleRules(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}
