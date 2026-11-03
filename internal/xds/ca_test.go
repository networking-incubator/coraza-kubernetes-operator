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

package xds

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCASourceLoadsLateMount pins the recovery path: a CA that appears after
// startup must become visible to anyone sharing the source, without a restart.
func TestCASourceLoadsLateMount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.crt")
	source := NewCASource(path, logr.Discard())

	require.Error(t, source.Ready())
	assert.Nil(t, source.PEM())

	_, _, caPEM := newTestCA(t)
	require.NoError(t, os.WriteFile(path, caPEM, 0o600))

	require.NoError(t, source.Ready())
	assert.Equal(t, caPEM, source.PEM())
}

// TestCASourceIgnoresUnparsableBundle keeps readiness failing while the file
// holds no certificate, instead of caching junk that the reconciler would
// inline into an EnvoyFilter.
func TestCASourceIgnoresUnparsableBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(path, []byte("not a certificate"), 0o600))

	source := NewCASource(path, logr.Discard())
	require.Error(t, source.Ready())
	assert.Nil(t, source.PEM())
}
