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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePMFromFileData(t *testing.T) {
	t.Run("no pmFromFile", func(t *testing.T) {
		assert.NoError(t, ValidatePMFromFileData(`SecRule REQUEST_URI "@rx x" "id:1,phase:1,pass"`, nil))
	})

	t.Run("pmFromFile without data", func(t *testing.T) {
		err := ValidatePMFromFileData(
			`SecRule ARGS "@pmFromFile overlap.data" "id:1,phase:1,pass"`,
			nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "overlap.data")
		assert.Contains(t, err.Error(), "spec.data")
	})

	t.Run("pmFromFile with matching data", func(t *testing.T) {
		err := ValidatePMFromFileData(
			`SecRule ARGS "@pmFromFile overlap.data" "id:1,phase:1,pass"`,
			map[string][]byte{"overlap.data": []byte("a")},
		)
		assert.NoError(t, err)
	})

	t.Run("pmFromFile missing key in data", func(t *testing.T) {
		err := ValidatePMFromFileData(
			`SecRule ARGS "@pmFromFile want.data" "id:1,phase:1,pass"`,
			map[string][]byte{"other.data": []byte("a")},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "want.data")
	})

	populatedData := map[string][]byte{"overlap.data": []byte("x")}

	t.Run("CRS comment prose ignored", func(t *testing.T) {
		err := ValidatePMFromFileData(
			"# For performance reasons, the @pmFromFile operator is used",
			populatedData,
		)
		assert.NoError(t, err)
	})

	t.Run("CRS multi-line comment block ignored", func(t *testing.T) {
		err := ValidatePMFromFileData(
			"# - Rule 933150: ~234 words highly common to PHP injection payloads\n"+
				"#		These words are detected as a match directly using @pmFromFile.\n"+
				"#		For performance reasons, the @pmFromFile operator is used, and many functions from lesser",
			populatedData,
		)
		assert.NoError(t, err)
	})

	t.Run("CRS comment for ignored", func(t *testing.T) {
		err := ValidatePMFromFileData(
			"# @pmFromFile for flexibility and performance.",
			populatedData,
		)
		assert.NoError(t, err)
	})

	t.Run("multiline SecRule with pmFromFile preserved", func(t *testing.T) {
		err := ValidatePMFromFileData(
			"SecRule BODY \"@pmFromFile overlap.data\" \\\n"+
				" \"id:1,phase:1,pass\"",
			populatedData,
		)
		assert.NoError(t, err)
	})
}
