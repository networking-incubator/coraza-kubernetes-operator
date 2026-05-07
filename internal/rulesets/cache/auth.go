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
	"fmt"
	"strings"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

const (
	// CacheEngineSAPrefix is the prefix added to ServiceAccount names created for
	// cache client authentication. The SA name is "coraza-engine-<engineName>".
	CacheEngineSAPrefix = "coraza-engine-"

	// cacheAudiencePrefix is the prefix for cache server JWT token audiences.
	// The full audience is "coraza-cache:<namespace>/<rulesetName>".
	cacheAudiencePrefix = "coraza-cache:"

	// defaultAuthCacheTTL is the default time-to-live for cached authentication results.
	// Cached entries are evicted after this duration to force re-validation.
	defaultAuthCacheTTL = 5 * time.Minute

	// maxAuthCacheSize is the maximum number of entries in the auth cache.
	// This prevents unbounded memory growth if many unique tokens are presented.
	// In practice, the number of legitimate tokens is bounded by the number of
	// Engines × RuleSets, which should be well below this limit.
	maxAuthCacheSize = 10000
)

// authCacheEntry holds a cached authentication result with its expiry time.
type authCacheEntry struct {
	result    *AuthResult
	expiresAt time.Time
}

// TokenAuthenticator validates Kubernetes ServiceAccount JWT tokens
// using the TokenReview API. It caches successful authentication results
// to avoid calling the TokenReview API on every request.
// Only successful authentications are cached — failed attempts are never
// stored, preventing cache poisoning via invalid tokens.
type TokenAuthenticator struct {
	tokenReview authclient.TokenReviewInterface
	cache       sync.Map // map[string]*authCacheEntry (token -> cached result)
	cacheSize   int64    // current number of cached entries, guarded by cacheMu
	// cacheMu guards cacheSize and serializes evict-then-store sequences.
	// sync.Map handles concurrent access to individual entries, but cacheSize
	// is a plain int64 that must stay in sync with the map. Without this mutex,
	// concurrent goroutines could race on cacheSize (causing drift) or both
	// pass the size-limit check and store simultaneously (overshooting the limit).
	cacheMu  sync.Mutex
	cacheTTL time.Duration
}

// Audience returns the audience string for a given cache key (namespace/rulesetName).
// This audience is embedded in ServiceAccount tokens so the cache server can verify
// that a token is scoped to the specific RuleSet being accessed.
func Audience(cacheKey string) string {
	return cacheAudiencePrefix + cacheKey
}

// NewTokenAuthenticator creates a TokenAuthenticator that uses the given
// TokenReview client to validate tokens.
func NewTokenAuthenticator(tokenReview authclient.TokenReviewInterface) *TokenAuthenticator {
	return &TokenAuthenticator{
		tokenReview: tokenReview,
		cacheTTL:    defaultAuthCacheTTL,
	}
}

// AuthResult contains the result of a token authentication attempt.
type AuthResult struct {
	// Namespace is the namespace of the authenticated ServiceAccount.
	Namespace string
	// Name is the name of the authenticated ServiceAccount.
	Name string
}

// Authenticate validates the given JWT token against the Kubernetes TokenReview API.
// It checks:
//  1. The token is valid and authenticated
//  2. The token has the required audience (e.g., "coraza-cache:namespace/rulesetName")
//  3. The token belongs to a ServiceAccount (system:serviceaccount:namespace:name)
//
// Returns the ServiceAccount namespace and name on success.
func (a *TokenAuthenticator) Authenticate(ctx context.Context, token, audience string) (*AuthResult, error) {
	// Cache key incorporates the audience to prevent cross-audience cache hits.
	// Use ":" as separator — it cannot appear in a JWT token (base64url + dots).
	cacheKey := token + ":" + audience

	// Check cache first to avoid a TokenReview API call on every request.
	if val, ok := a.cache.Load(cacheKey); ok {
		entry := val.(*authCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return entry.result, nil
		}
		// Expired — atomically remove and decrement counter.
		a.cacheMu.Lock()
		if _, loaded := a.cache.LoadAndDelete(cacheKey); loaded {
			a.cacheSize--
		}
		a.cacheMu.Unlock()
	}

	review := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{audience},
		},
	}

	result, err := a.tokenReview.Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("token review failed: %w", err)
	}

	if !result.Status.Authenticated {
		return nil, fmt.Errorf("token not authenticated")
	}

	// Parse "system:serviceaccount:namespace:name" from the username
	namespace, name, err := parseServiceAccountUsername(result.Status.User.Username)
	if err != nil {
		return nil, err
	}

	authResult := &AuthResult{
		Namespace: namespace,
		Name:      name,
	}

	expiresAt := time.Now().Add(a.cacheTTL)

	a.cacheMu.Lock()
	// Proactive eviction at 90% capacity to prevent the cache from staying
	// perpetually full once it reaches maxAuthCacheSize.
	if a.cacheSize >= maxAuthCacheSize*9/10 {
		a.evictExpiredLocked()
	}
	if a.cacheSize < maxAuthCacheSize {
		a.cache.Store(cacheKey, &authCacheEntry{
			result:    authResult,
			expiresAt: expiresAt,
		})
		a.cacheSize++
	}
	a.cacheMu.Unlock()

	return authResult, nil
}

// evictExpiredLocked removes expired entries from the cache.
// Must be called with cacheMu held.
func (a *TokenAuthenticator) evictExpiredLocked() {
	now := time.Now()
	a.cache.Range(func(key, value any) bool {
		entry := value.(*authCacheEntry)
		if now.After(entry.expiresAt) {
			a.cache.Delete(key)
			a.cacheSize--
		}
		return true
	})
}

// parseServiceAccountUsername extracts namespace and name from a
// "system:serviceaccount:<namespace>:<name>" username string.
func parseServiceAccountUsername(username string) (namespace, name string, err error) {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, prefix) {
		return "", "", fmt.Errorf("not a service account token: %s", username)
	}

	nsName := strings.SplitN(strings.TrimPrefix(username, prefix), ":", 2)
	if len(nsName) != 2 {
		return "", "", fmt.Errorf("invalid service account username format: %s", username)
	}

	return nsName[0], nsName[1], nil
}
