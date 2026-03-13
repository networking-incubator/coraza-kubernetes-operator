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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const skipCountAssertion = -1

var testDataFile = map[string][]byte{
	"something1.data": []byte("xpto blabla"),
	"something2.data": []byte("another weird data"),
}

func TestRuleSetCache_PutAndGet(t *testing.T) {
	cache := NewRuleSetCache()

	tests := []struct {
		name      string
		instance  string
		rules     string
		dataFiles map[string][]byte
	}{
		{
			name:     "simple rules",
			instance: "test-instance",
			rules:    "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\"",
		},
		{
			name:     "empty rules",
			instance: "empty-instance",
			rules:    "",
		},
		{
			name:     "multi-line rules",
			instance: "multi-instance",
			rules:    "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\"\nSecRule REQUEST_URI \"@contains /api\" \"id:2,deny\"",
		},
		{
			name:      "multi-line rules and datafiles",
			instance:  "multi-instance",
			rules:     "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\"\nSecRule REQUEST_URI \"@contains /api\" \"id:2,deny\"",
			dataFiles: testDataFile,
		},
		{
			name:      "multi-line rules and empty",
			instance:  "multi-instance",
			rules:     "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\"\nSecRule REQUEST_URI \"@contains /api\" \"id:2,deny\"",
			dataFiles: map[string][]byte{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache.Put(tt.instance, tt.rules, tt.dataFiles)

			entry, ok := cache.Get(tt.instance)
			require.True(t, ok, "Entry should exist")
			require.NotNil(t, entry)

			assert.Equal(t, tt.rules, entry.Rules)
			if len(tt.dataFiles) > 0 {
				assert.Equal(t, tt.dataFiles, entry.DataFiles)
			} else {
				assert.Empty(t, entry.DataFiles)
			}

			assert.NotEmpty(t, entry.UUID, "UUID should be generated")
			assert.False(t, entry.Timestamp.IsZero(), "Timestamp should be set")
		})
	}
}

func TestRuleSetCache_Pruning(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(*RuleSetCache)
		pruneMaxAge   time.Duration
		pruneMaxSize  int
		expectedCount int
		verifyLatest  func(*testing.T, *RuleSetCache)
	}{
		{
			name: "prune old entries by age",
			setup: func(c *RuleSetCache) {
				c.Put("instance1", "old-rules", nil)
				c.Put("instance1", "new-rules", nil)
				c.Put("instance2", "rules2", nil)
				c.SetEntryTimestamp("instance1", 0, time.Now().Add(-25*time.Hour))
			},
			pruneMaxAge:   24 * time.Hour,
			expectedCount: 1,
			verifyLatest: func(t *testing.T, c *RuleSetCache) {
				entry, ok := c.Get("instance1")
				require.True(t, ok)
				assert.Equal(t, "new-rules", entry.Rules)
			},
		},
		{
			name: "prune nothing when all entries are recent",
			setup: func(c *RuleSetCache) {
				c.Put("instance1", "rules1", nil)
				c.Put("instance2", "rules2", nil)
			},
			pruneMaxAge:   48 * time.Hour,
			expectedCount: 0,
		},
		{
			name: "prune by size",
			setup: func(c *RuleSetCache) {
				c.Put("instance1", "rules1", nil)
				c.Put("instance1", "new1", nil)
				c.Put("instance2", "rules2", nil)
				c.Put("instance2", "new2", nil)
				c.Put("instance3", "rules3", nil)
				c.Put("instance4", "rules4", testDataFile)
				c.SetEntryTimestamp("instance1", 0, time.Now().Add(-2*time.Hour))
				c.SetEntryTimestamp("instance2", 0, time.Now().Add(-1*time.Hour))
				c.SetEntryTimestamp("instance4", 0, time.Now().Add(-2*time.Hour))
			},
			pruneMaxSize:  80,
			expectedCount: skipCountAssertion,
			verifyLatest: func(t *testing.T, c *RuleSetCache) {
				assert.LessOrEqual(t, c.TotalSize(), 80)
				_, ok := c.Get("instance1")
				assert.True(t, ok)
				_, ok = c.Get("instance2")
				assert.True(t, ok)
				_, ok = c.Get("instance3")
				assert.True(t, ok)
				_, ok = c.Get("instance4")
				assert.True(t, ok)
			},
		},
		{
			name: "prune by size under limit does nothing",
			setup: func(c *RuleSetCache) {
				c.Put("instance1", "rules1", nil)
				c.Put("instance2", "rules2", nil)
			},
			pruneMaxSize:  1000,
			expectedCount: 0,
		},
		{
			name: "never prune latest entry by age",
			setup: func(c *RuleSetCache) {
				c.Put("instance1", "v1", nil)
				time.Sleep(10 * time.Millisecond)
				c.Put("instance1", "v2", nil)
				time.Sleep(10 * time.Millisecond)
				c.Put("instance1", "v3", nil)
				for i := range 3 {
					c.SetEntryTimestamp("instance1", i, time.Now().Add(-48*time.Hour))
				}
			},
			pruneMaxAge:   24 * time.Hour,
			expectedCount: 2,
			verifyLatest: func(t *testing.T, c *RuleSetCache) {
				entry, ok := c.Get("instance1")
				require.True(t, ok)
				assert.Equal(t, "v3", entry.Rules)
			},
		},
		{
			name: "never prune latest entry by size",
			setup: func(c *RuleSetCache) {
				c.Put("instance1", "small", nil)
				time.Sleep(10 * time.Millisecond)
				c.Put("instance1", "medium-size", nil)
				time.Sleep(10 * time.Millisecond)
				c.Put("instance1", "this-is-a-much-larger-entry", testDataFile)
			},
			pruneMaxSize:  1,
			expectedCount: 2,
			verifyLatest: func(t *testing.T, c *RuleSetCache) {
				entry, ok := c.Get("instance1")
				require.True(t, ok)
				assert.Equal(t, "this-is-a-much-larger-entry", entry.Rules)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := NewRuleSetCache()
			tt.setup(cache)

			var pruned int
			if tt.pruneMaxSize > 0 {
				t.Logf("Pruning by size (max: %d bytes)", tt.pruneMaxSize)
				pruned = cache.PruneBySize(tt.pruneMaxSize)
			} else {
				t.Logf("Pruning by age (max: %v)", tt.pruneMaxAge)
				pruned = cache.Prune(tt.pruneMaxAge)
			}

			if tt.expectedCount >= 0 {
				assert.Equal(t, tt.expectedCount, pruned)
			} else {
				t.Logf("Pruned %d entries (count not verified)", pruned)
			}

			if tt.verifyLatest != nil {
				tt.verifyLatest(t, cache)
			}
		})
	}
}

func TestRuleSetCache_ListKeys(t *testing.T) {
	cache := NewRuleSetCache()
	keys := cache.ListKeys()
	assert.Empty(t, keys)
	cache.Put("instance1", "rules1", map[string][]byte{
		"something.data": []byte("somedata"),
	})
	cache.Put("instance2", "rules2", nil)
	cache.Put("instance3", "rules3", map[string][]byte{
		"something.data": []byte("another data"),
	})
	keys = cache.ListKeys()
	assert.Len(t, keys, 3)
	assert.ElementsMatch(t, []string{"instance1", "instance2", "instance3"}, keys)
}

func TestRuleSetCache_TotalSize(t *testing.T) {
	cache := NewRuleSetCache()
	assert.Equal(t, 0, cache.TotalSize())
	cache.Put("instance1", "12345", nil)
	cache.Put("instance2", "1234567890", nil)
	assert.Equal(t, 15, cache.TotalSize())
	cache.Put("instance1", "123", nil)
	assert.Equal(t, 18, cache.TotalSize())

	// Adds 18 (previous) + 5 (rule) + 5 (filename) + 5 (filecontent)
	cache.Put("instance3", "12345", map[string][]byte{
		"file1": []byte("abcde"),
	})
	assert.Equal(t, 33, cache.TotalSize())
}

func TestRuleSetCache_PutUpdatesUUID(t *testing.T) {
	cache := NewRuleSetCache()
	instance := "test-instance"
	cache.Put(instance, "rules v1", nil)
	entry1, _ := cache.Get(instance)
	time.Sleep(10 * time.Millisecond)
	cache.Put(instance, "rules v2", nil)
	entry2, _ := cache.Get(instance)
	assert.NotEqual(t, entry1.UUID, entry2.UUID, "UUID should change on update")
	assert.NotEqual(t, entry1.Timestamp, entry2.Timestamp, "Timestamp should change on update")
	assert.Equal(t, "rules v2", entry2.Rules)
}

func TestRuleSetCache_GetNonExistent(t *testing.T) {
	cache := NewRuleSetCache()
	entry, ok := cache.Get("non-existent")
	assert.False(t, ok)
	assert.Nil(t, entry)
	assert.Zero(t, cache.CountEntries("non-existent"))
}
