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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTokenEntry_RenewalDeadline(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		entry    TokenEntry
		expected time.Time
	}{
		{
			name: "1h token renews at 48min",
			entry: TokenEntry{
				IssuedAt:  now,
				ExpiresAt: now.Add(1 * time.Hour),
			},
			expected: now.Add(48 * time.Minute), // 0.8 * 60min
		},
		{
			name: "30min token renews at 24min",
			entry: TokenEntry{
				IssuedAt:  now,
				ExpiresAt: now.Add(30 * time.Minute),
			},
			expected: now.Add(24 * time.Minute), // 0.8 * 30min
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deadline := tt.entry.RenewalDeadline()
			// Allow 1ms tolerance for floating-point duration arithmetic.
			assert.WithinDuration(t, tt.expected, deadline, time.Millisecond)
		})
	}
}

func TestTokenEntry_NeedsRenewal(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		entry    TokenEntry
		expected bool
	}{
		{
			name: "fresh token does not need renewal",
			entry: TokenEntry{
				IssuedAt:  now,
				ExpiresAt: now.Add(1 * time.Hour),
			},
			expected: false,
		},
		{
			name: "token at 50% lifetime does not need renewal",
			entry: TokenEntry{
				IssuedAt:  now.Add(-30 * time.Minute),
				ExpiresAt: now.Add(30 * time.Minute), // 1h total
			},
			expected: false,
		},
		{
			name: "token at 80% lifetime needs renewal",
			entry: TokenEntry{
				IssuedAt:  now.Add(-48 * time.Minute),
				ExpiresAt: now.Add(12 * time.Minute), // 1h total, 80% elapsed
			},
			expected: true,
		},
		{
			name: "expired token needs renewal",
			entry: TokenEntry{
				IssuedAt:  now.Add(-2 * time.Hour),
				ExpiresAt: now.Add(-1 * time.Hour),
			},
			expected: true,
		},
		{
			name: "short-lived token (1h) at 50min does need renewal",
			entry: TokenEntry{
				IssuedAt:  now.Add(-50 * time.Minute),
				ExpiresAt: now.Add(10 * time.Minute), // 1h total
			},
			expected: true, // 50/60 = 83% > 80%
		},
		{
			name: "short-lived token (1h) at 40min does not need renewal",
			entry: TokenEntry{
				IssuedAt:  now.Add(-40 * time.Minute),
				ExpiresAt: now.Add(20 * time.Minute), // 1h total
			},
			expected: false, // 40/60 = 67% < 80%
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.entry.NeedsRenewal())
		})
	}
}

func TestPruneExpiredTokens(t *testing.T) {
	r := &EngineReconciler{}
	now := time.Now()

	// Add some entries: 2 expired, 1 valid. Keyed by namespace/engineName/rulesetName.
	r.tokenStore.Store("ns1/engine1/ruleset-a", &TokenEntry{
		Token:     "expired-1",
		IssuedAt:  now.Add(-10 * 24 * time.Hour),
		ExpiresAt: now.Add(-5 * 24 * time.Hour),
	})
	r.tokenStore.Store("ns2/engine2/ruleset-b", &TokenEntry{
		Token:     "expired-2",
		IssuedAt:  now.Add(-2 * 24 * time.Hour),
		ExpiresAt: now.Add(-1 * time.Hour),
	})
	r.tokenStore.Store("ns3/engine3/ruleset-c", &TokenEntry{
		Token:     "valid",
		IssuedAt:  now,
		ExpiresAt: now.Add(1 * time.Hour),
	})

	r.pruneExpiredTokens()

	_, ok1 := r.tokenStore.Load("ns1/engine1/ruleset-a")
	_, ok2 := r.tokenStore.Load("ns2/engine2/ruleset-b")
	_, ok3 := r.tokenStore.Load("ns3/engine3/ruleset-c")

	assert.False(t, ok1, "expired entry ns1/engine1/ruleset-a should be pruned")
	assert.False(t, ok2, "expired entry ns2/engine2/ruleset-b should be pruned")
	assert.True(t, ok3, "valid entry ns3/engine3/ruleset-c should remain")
}

func TestPruneExpiredTokens_EmptyStore(t *testing.T) {
	r := &EngineReconciler{}
	// Should not panic on empty store.
	r.pruneExpiredTokens()

	count := 0
	r.tokenStore.Range(func(_, _ any) bool {
		count++
		return true
	})
	assert.Equal(t, 0, count)
}

func TestPruneExpiredTokens_AllExpired(t *testing.T) {
	r := &EngineReconciler{}
	now := time.Now()

	r.tokenStore.Store("ns/e1/rs1", &TokenEntry{
		Token:     "t1",
		IssuedAt:  now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-1 * time.Hour),
	})
	r.tokenStore.Store("ns/e2/rs2", &TokenEntry{
		Token:     "t2",
		IssuedAt:  now.Add(-3 * time.Hour),
		ExpiresAt: now.Add(-30 * time.Minute),
	})

	r.pruneExpiredTokens()

	count := 0
	r.tokenStore.Range(func(_, _ any) bool {
		count++
		return true
	})
	assert.Equal(t, 0, count, "all expired entries should be pruned")
}

func TestPruneExpiredTokens_NoneExpired(t *testing.T) {
	r := &EngineReconciler{}
	now := time.Now()

	r.tokenStore.Store("ns/e1/rs1", &TokenEntry{
		Token:     "t1",
		IssuedAt:  now,
		ExpiresAt: now.Add(1 * time.Hour),
	})
	r.tokenStore.Store("ns/e2/rs2", &TokenEntry{
		Token:     "t2",
		IssuedAt:  now,
		ExpiresAt: now.Add(30 * time.Minute),
	})

	r.pruneExpiredTokens()

	count := 0
	r.tokenStore.Range(func(_, _ any) bool {
		count++
		return true
	})
	assert.Equal(t, 2, count, "no entries should be pruned")
}

func TestCleanupStaleTokens(t *testing.T) {
	r := &EngineReconciler{}
	now := time.Now()

	r.tokenStore.Store("ns/engine1/old-ruleset", &TokenEntry{
		Token:     "old",
		IssuedAt:  now,
		ExpiresAt: now.Add(1 * time.Hour),
	})
	r.tokenStore.Store("ns/engine1/current-ruleset", &TokenEntry{
		Token:     "current",
		IssuedAt:  now,
		ExpiresAt: now.Add(1 * time.Hour),
	})
	r.tokenStore.Store("ns/engine2/other-ruleset", &TokenEntry{
		Token:     "unrelated",
		IssuedAt:  now,
		ExpiresAt: now.Add(1 * time.Hour),
	})

	r.cleanupStaleTokens("ns", "engine1", "current-ruleset")

	_, hasOld := r.tokenStore.Load("ns/engine1/old-ruleset")
	assert.False(t, hasOld, "stale token for old RuleSet should be removed")

	_, hasCurrent := r.tokenStore.Load("ns/engine1/current-ruleset")
	assert.True(t, hasCurrent, "token for current RuleSet should remain")

	_, hasUnrelated := r.tokenStore.Load("ns/engine2/other-ruleset")
	assert.True(t, hasUnrelated, "token for a different Engine should remain")
}

func TestCleanupStaleTokens_NoStaleEntries(t *testing.T) {
	r := &EngineReconciler{}
	now := time.Now()

	r.tokenStore.Store("ns/engine1/current", &TokenEntry{
		Token:     "t1",
		IssuedAt:  now,
		ExpiresAt: now.Add(1 * time.Hour),
	})

	r.cleanupStaleTokens("ns", "engine1", "current")

	count := 0
	r.tokenStore.Range(func(_, _ any) bool {
		count++
		return true
	})
	assert.Equal(t, 1, count, "no entries should be removed when none are stale")
}

func TestCleanupStaleTokens_EmptyStore(t *testing.T) {
	r := &EngineReconciler{}
	r.cleanupStaleTokens("ns", "engine1", "current")

	count := 0
	r.tokenStore.Range(func(_, _ any) bool {
		count++
		return true
	})
	assert.Equal(t, 0, count)
}

func TestCacheClientSALabels(t *testing.T) {
	labels := cacheClientSALabels("my-engine")
	assert.Equal(t, "coraza-kubernetes-operator", labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "cache-client", labels["app.kubernetes.io/component"])
	assert.Equal(t, "my-engine", labels["app.kubernetes.io/instance"])

	// Different engine names produce different labels.
	labels2 := cacheClientSALabels("other-engine")
	assert.NotEqual(t, labels["app.kubernetes.io/instance"], labels2["app.kubernetes.io/instance"])
}
