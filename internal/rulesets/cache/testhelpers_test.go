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
)

// setEntryTimestamp updates the timestamp of a stored entry by index.
// Only for use in tests to manipulate entry ages for GC testing.
func setEntryTimestamp(c *RuleSetCache, instance string, index int, timestamp time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entries, ok := c.entries[instance]; ok {
		if index >= 0 && index < len(entries.Entries) {
			entries.Entries[index].Timestamp = timestamp
		}
	}
}

// resetGCMetrics resets the package-level GC counters before a test and
// registers a cleanup that resets them again after the test completes.
// Call at the start of any test that reads or asserts on the GC counters.
func resetGCMetrics(t *testing.T) {
	t.Helper()
	gcPrunedEntriesTotal.Reset()
	gcSizeLimitExceededTotal.Reset()
	t.Cleanup(func() {
		gcPrunedEntriesTotal.Reset()
		gcSizeLimitExceededTotal.Reset()
	})
}
