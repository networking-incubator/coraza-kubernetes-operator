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

// Package cache provides in-memory caching for WAF rulesets.
package cache

import (
	"bytes"
	"sync"
	"time"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------------
// RuleSetEntry
// -----------------------------------------------------------------------------

// RuleSetEntry represents a cached ruleset with metadata
type RuleSetEntry struct {
	UUID      string    `json:"uuid"`
	Timestamp time.Time `json:"timestamp"`
	Rules     string    `json:"rules"`
	// DataFiles contains a map with the data file names and their contents
	DataFiles map[string][]byte `json:"dataFiles,omitempty,omitzero"`
}

// RuleSetEntries wraps a list of RuleSetEntry objects for an instance.
// Entries are ordered oldest to newest. Latest entry is marked.
type RuleSetEntries struct {
	Latest  string          `json:"latest"`
	Entries []*RuleSetEntry `json:"entries"`
}

// -----------------------------------------------------------------------------
// RuleSetCache
// -----------------------------------------------------------------------------

// RuleSetCache provides thread-safe storage for rulesets with versioning
type RuleSetCache struct {
	mu           sync.RWMutex
	entries      map[string]*RuleSetEntries
	totalSize    int
	totalEntries int
}

// NewRuleSetCache creates a new RuleSetCache instance
func NewRuleSetCache() *RuleSetCache {
	return &RuleSetCache{
		entries: make(map[string]*RuleSetEntries),
	}
}

// Get retrieves the latest ruleset entry for the given instance
func (c *RuleSetCache) Get(instance string) (*RuleSetEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries, ok := c.entries[instance]
	if ok && len(entries.Entries) > 0 {
		// Find the entry matching the Latest UUID.
		for _, entry := range entries.Entries {
			if entry.UUID == entries.Latest {
				// Return a deep copy so callers cannot mutate internal cache state.
				var copiedDataFiles map[string][]byte
				if entry.DataFiles != nil {
					copiedDataFiles = make(map[string][]byte, len(entry.DataFiles))
					for name, contents := range entry.DataFiles {
						copiedDataFiles[name] = bytes.Clone(contents)
					}
				}
				copiedEntry := &RuleSetEntry{
					UUID:      entry.UUID,
					Timestamp: entry.Timestamp,
					Rules:     entry.Rules,
					DataFiles: copiedDataFiles,
				}
				return copiedEntry, true
			}
		}
	}

	return nil, false
}

// Put stores rules for the given instance with a new UUID and timestamp.
// New entries are appended to the end, maintaining oldest-to-newest order.
func (c *RuleSetCache) Put(instance string, rules string, datafiles map[string][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Deep-copy to avoid races if the caller mutates the map after Put returns.
	var internalData map[string][]byte
	if len(datafiles) > 0 {
		internalData = make(map[string][]byte, len(datafiles))
		for f, v := range datafiles {
			internalData[f] = bytes.Clone(v)
		}
	}

	newEntry := &RuleSetEntry{
		UUID:      uuid.New().String(),
		Timestamp: time.Now(),
		Rules:     rules,
		DataFiles: internalData,
	}
	newEntrySize := entrySize(newEntry)

	if c.entries[instance] == nil {
		c.entries[instance] = &RuleSetEntries{
			Latest:  newEntry.UUID,
			Entries: []*RuleSetEntry{newEntry},
		}
	} else {
		c.entries[instance].Entries = append(c.entries[instance].Entries, newEntry)
		c.entries[instance].Latest = newEntry.UUID
	}
	c.totalSize += newEntrySize
	c.totalEntries++
}

// Len returns the number of instances stored in the cache
func (c *RuleSetCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// ListKeys returns all instance names stored in the cache
func (c *RuleSetCache) ListKeys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.entries))
	for k := range c.entries {
		keys = append(keys, k)
	}
	return keys
}

// TotalSize returns the total size of all cached rules in bytes
func (c *RuleSetCache) TotalSize() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.totalSize
}

// TotalEntries returns the total number of stored entry revisions across all cache keys.
func (c *RuleSetCache) TotalEntries() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.totalEntries
}

// CountEntries returns the number of entries for an instance.
func (c *RuleSetCache) CountEntries(instance string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if entries, ok := c.entries[instance]; ok {
		return len(entries.Entries)
	}
	return 0
}

// -----------------------------------------------------------------------------
// RuleSetCache - Cleanup
// -----------------------------------------------------------------------------

// Prune removes cache entries older than the specified age, but never removes
// the latest entry for any instance
func (c *RuleSetCache) Prune(maxAge time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) == 0 {
		return 0
	}

	pruned := 0
	now := time.Now()
	for instance, entries := range c.entries {
		newEntries := make([]*RuleSetEntry, 0, len(entries.Entries))
		for _, entry := range entries.Entries {
			if entry.UUID == entries.Latest {
				newEntries = append(newEntries, entry)
				continue // never prune latest
			}

			if now.Sub(entry.Timestamp) <= maxAge {
				newEntries = append(newEntries, entry)
			} else {
				c.totalSize -= entrySize(entry)
				c.totalEntries--
				pruned++
			}
		}
		c.entries[instance].Entries = newEntries
	}

	return pruned
}

// PruneBySize removes oldest entries until cache is under maxSize. Iterates instances,
// pruning from oldest to newest, but never removes the latest entry for any instance.
// Will log errors if the cache size cannot be reduced under maxSize.
func (c *RuleSetCache) PruneBySize(maxSize int) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	currentSize := c.totalSize

	if currentSize <= maxSize {
		return 0
	}

	// Prune oldest entries from each instance until under size limit
	// Entries are already ordered oldest to newest, so we can prune from the front
	pruned := 0
	for instance, entries := range c.entries {
		if currentSize <= maxSize {
			break
		}

		newEntries := make([]*RuleSetEntry, 0, len(entries.Entries))
		for _, entry := range entries.Entries {
			if entry.UUID == entries.Latest {
				newEntries = append(newEntries, entry)
				continue // never prune latest
			}

			// If we're still over size, prune.
			if currentSize > maxSize {
				removedSize := entrySize(entry)
				currentSize -= removedSize
				c.totalSize -= removedSize
				c.totalEntries--
				pruned++
			} else {
				// Under size now, keep the remainder.
				newEntries = append(newEntries, entry)
			}
		}
		c.entries[instance].Entries = newEntries
	}

	return pruned
}

// entrySize computes the byte size of an entry's payload.
// Incremental totalSize and totalEntries accounting in Put/Prune/PruneBySize
// depends on entries being immutable after creation — never mutate Rules or
// DataFiles on a stored entry.
func entrySize(entry *RuleSetEntry) int {
	size := len(entry.Rules)
	for filename, v := range entry.DataFiles {
		size += len(filename)
		size += len(v)
	}
	return size
}
