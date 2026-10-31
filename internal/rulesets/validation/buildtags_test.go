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
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManagerBuildDisablesMemoizer guards the shipped manager build tags.
// coraza.NewWAF registers its memoizer before parsing and returns a nil WAF on a
// parse error, so CloseWAF cannot release it: every failed validation of new
// content leaks its owner, which a tenant can trigger repeatedly. The tag
// selects Coraza's no-op memoize variant (internal/memoize/noop.go), and the
// operator runs no WAF transactions, so nothing loses caching. Dropping the tag
// would silently reinstate the leak; it is asserted here because a build does
// not fail on an unknown tag.
func TestManagerBuildDisablesMemoizer(t *testing.T) {
	for _, file := range []string{"../../../Dockerfile", "../../../Makefile"} {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)

		found := 0
		for line := range strings.SplitSeq(string(raw), "\n") {
			if !strings.Contains(line, "-o manager") && !strings.Contains(line, "-o bin/manager") {
				continue
			}
			found++
			assert.Contains(t, line, "coraza.no_memoize", "%s must build the manager with coraza.no_memoize: %s", file, line)
		}
		assert.NotZero(t, found, "%s must build the manager", file)
	}
}
