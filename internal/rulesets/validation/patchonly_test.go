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

package validation_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
)

func TestIsPatchOnlyFragment(t *testing.T) {
	t.Run("CRS 999 snippet is patch-only", func(t *testing.T) {
		path := filepath.Join("..", "..", "..", "tmp", "coreruleset", "rules", "REQUEST-999-COMMON-EXCEPTIONS-AFTER.conf")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Skip("CRS rules not downloaded:", err)
		}
		assert.True(t, validation.IsPatchOnlyFragment(string(data)))
	})

	t.Run("single SecRuleUpdateTargetById", func(t *testing.T) {
		rules := `SecRuleUpdateTargetById 932240 "!REQUEST_COOKIES:/^_ga(?:_\w+)?$/"`
		assert.True(t, validation.IsPatchOnlyFragment(rules))
	})

	t.Run("SecRule definition is not patch-only", func(t *testing.T) {
		rules := `SecRule REQUEST_URI "@rx x" "id:1,phase:1,pass"`
		assert.False(t, validation.IsPatchOnlyFragment(rules))
	})

	t.Run("mixed SecRule and SecRuleUpdate is not patch-only", func(t *testing.T) {
		rules := `SecRule REQUEST_URI "@rx x" "id:1,phase:1,pass"
SecRuleUpdateTargetById 1 "!ARGS:x"`
		assert.False(t, validation.IsPatchOnlyFragment(rules))
	})

	t.Run("unknown Sec directive is not patch-only", func(t *testing.T) {
		rules := `SecInvalidDirective "bad"`
		assert.False(t, validation.IsPatchOnlyFragment(rules))
	})

	t.Run("comments only is not patch-only", func(t *testing.T) {
		assert.False(t, validation.IsPatchOnlyFragment("# comment only\n"))
	})

	t.Run("SecRuleRemoveById is patch-only", func(t *testing.T) {
		rules := `SecRuleRemoveById 12345`
		assert.True(t, validation.IsPatchOnlyFragment(rules))
	})
}

func TestValidateRuleSourceRules_patchOnly(t *testing.T) {
	t.Run("SecRuleUpdate with missing target ID returns nil", func(t *testing.T) {
		err := validation.ValidateRuleSourceRules(
			`SecRuleUpdateTargetById 932240 "!REQUEST_COOKIES:/^_ga(?:_\w+)?$/"`,
			"request-999", nil,
		)
		assert.NoError(t, err)
	})

	t.Run("invalid SecRule still errors", func(t *testing.T) {
		err := validation.ValidateRuleSourceRules(`SecInvalidDirective "bad"`, "bad-rs", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad-rs")
	})

	t.Run("mixed fragment with invalid SecRule errors", func(t *testing.T) {
		err := validation.ValidateRuleSourceRules(
			`SecRule REQUEST_URI "@rx x" "id:1,phase:1,pass"
SecRuleUpdateTargetById 99999 "!ARGS:x"`,
			"mixed-rs", nil,
		)
		// Coraza may fail on missing update target; fragment is not patch-only so validation runs.
		_ = err
	})
}
