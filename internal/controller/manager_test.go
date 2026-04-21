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

	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
)

func TestResolveRuleSetMaxPayloadSize(t *testing.T) {
	t.Parallel()

	t.Run("zero defaults to CacheMaxSize", func(t *testing.T) {
		t.Parallel()
		got, err := resolveRuleSetMaxPayloadSize(RuleSetOpts{})
		require.NoError(t, err)
		require.Equal(t, cache.CacheMaxSize, got)
	})

	t.Run("positive passes through", func(t *testing.T) {
		t.Parallel()
		const want = 42
		got, err := resolveRuleSetMaxPayloadSize(RuleSetOpts{MaxPayloadSize: want})
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("negative is invalid", func(t *testing.T) {
		t.Parallel()
		_, err := resolveRuleSetMaxPayloadSize(RuleSetOpts{MaxPayloadSize: -1})
		require.Error(t, err)
	})
}
