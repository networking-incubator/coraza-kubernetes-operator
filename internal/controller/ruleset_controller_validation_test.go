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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateConfigMapRules(t *testing.T) {
	t.Run("valid rules return nil", func(t *testing.T) {
		err := validateConfigMapRules(`SecDefaultAction "phase:1,log,auditlog,pass"`, "test-cm", nil)
		assert.NoError(t, err)
	})

	t.Run("invalid rules return error mentioning ConfigMap name", func(t *testing.T) {
		err := validateConfigMapRules(`SecInvalidDirective "bad"`, "bad-cm", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad-cm")
		assert.Contains(t, err.Error(), "doesn't contain valid rules")
	})

	t.Run("missing file error is skipped when file exists in secretData", func(t *testing.T) {
		secretData := map[string][]byte{"rule1.data": []byte("content")}
		err := validateConfigMapRules(
			`SecRule REQUEST_URI "@pmFromFile rule1.data" "id:1,phase:1,deny"`,
			"data-cm", secretData,
		)
		assert.NoError(t, err)
	})

	t.Run("missing file error is reported when file not in secretData", func(t *testing.T) {
		err := validateConfigMapRules(
			`SecRule REQUEST_URI "@pmFromFile missing.data" "id:1,phase:1,deny"`,
			"data-cm", nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "data-cm")
		// Adversarial: user-visible error must not embed memfs/container absolute paths.
		msg := err.Error()
		if strings.Contains(msg, "/") {
			for _, leak := range []string{"/var/", "/etc/", "/tmp/", "/app/", "/root/"} {
				assert.NotContains(t, msg, leak, "validation error leaked a filesystem path segment")
			}
		}
	})
}
