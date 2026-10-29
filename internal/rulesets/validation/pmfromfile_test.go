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

	t.Run("pmf alias without data", func(t *testing.T) {
		err := ValidatePMFromFileData(
			`SecRule ARGS "@pmf /etc/passwd" "id:1,phase:1,pass"`,
			nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/etc/passwd")
		assert.Contains(t, err.Error(), "spec.data")
	})

	t.Run("pmf alias with matching data", func(t *testing.T) {
		err := ValidatePMFromFileData(
			`SecRule ARGS "@pmf overlap.data" "id:1,phase:1,pass"`,
			map[string][]byte{"overlap.data": []byte("a")},
		)
		assert.NoError(t, err)
	})

	// Coraza path-cleans the argument, so a leading ./ must resolve the same
	// RuleData file instead of being rejected.
	for _, argument := range []string{"./overlap.data", ".//overlap.data"} {
		t.Run("pmf "+argument+" resolves the clean name", func(t *testing.T) {
			assert.NoError(t, ValidatePMFromFileData(
				`SecRule ARGS "@pmf `+argument+`" "id:1,phase:1,pass"`,
				map[string][]byte{"overlap.data": []byte("a")},
			))
			err := ValidatePMFromFileData(
				`SecRule ARGS "@pmf `+argument+`" "id:1,phase:1,pass"`,
				map[string][]byte{"other.data": []byte("a")},
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "overlap.data")
		})
	}

	// The plugin serves the embedded CRS tree, so crs/... targets do not need
	// RuleData. A typo there is a delivery risk the operator cannot verify.
	t.Run("pmf embedded crs data is accepted without RuleData", func(t *testing.T) {
		for _, dataFiles := range []map[string][]byte{nil, {}, {"other.data": []byte("x")}} {
			assert.NoError(t, ValidatePMFromFileData(
				`SecRule ARGS "@pmf crs/restricted-files.data" "id:1,phase:1,pass"`,
				dataFiles,
			))
		}
	})

	t.Run("pmf traversal out of the embedded crs tree still needs RuleData", func(t *testing.T) {
		err := ValidatePMFromFileData(
			`SecRule ARGS "@pmf crs/../../etc/passwd" "id:1,phase:1,pass"`,
			map[string][]byte{},
		)
		require.Error(t, err)
	})

	populatedData := map[string][]byte{"overlap.data": []byte("x")}

	for _, operator := range []string{"@pmFromFile", "@pmf"} {
		t.Run(operator+" rejects multiple file names", func(t *testing.T) {
			err := ValidatePMFromFileData(
				`SecRule ARGS "`+operator+` overlap.data other.data" "id:1,phase:1,pass"`,
				map[string][]byte{"overlap.data": []byte("x"), "other.data": []byte("y")},
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "single data file name")
			assert.Contains(t, err.Error(), "overlap.data other.data")
		})
	}

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

	// Coraza joins a trailing-backslash continuation, so the scan must too or
	// the operator file reference hides from the guard.
	t.Run("continuation cannot hide a pmf argument", func(t *testing.T) {
		err := ValidatePMFromFileData(
			"SecRule ARGS \"@pmf \\\n/etc/passwd\" \"id:1,phase:1,pass\"",
			nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/etc/passwd")
	})

	t.Run("CRLF continuation cannot hide a pmf argument", func(t *testing.T) {
		err := ValidatePMFromFileData(
			"SecRule ARGS \"@pmf \\\r\n/etc/passwd\" \"id:1,phase:1,pass\"",
			nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/etc/passwd")
	})

	t.Run("continuation with a provided data file is accepted", func(t *testing.T) {
		err := ValidatePMFromFileData(
			"SecRule ARGS \"@pmFromFile \\\noverlap.data\" \"id:1,phase:1,pass\"",
			populatedData,
		)
		assert.NoError(t, err)
	})

	// Both are legitimate Coraza rules: @pmf only counts as an operator when it
	// starts the quoted rule argument.
	t.Run("@pmf inside another operator is not an operator", func(t *testing.T) {
		assert.NoError(t, ValidatePMFromFileData(
			`SecRule ARGS "@rx @pmf foo" "id:1,phase:1,pass"`,
			nil,
		))
	})

	t.Run("@pmf inside an action message is not an operator", func(t *testing.T) {
		assert.NoError(t, ValidatePMFromFileData(
			`SecRule ARGS "@rx x" "id:1,phase:1,pass,msg:'use @pmf foo'"`,
			nil,
		))
	})

	t.Run("leading whitespace inside the quotes is not a pmf operator", func(t *testing.T) {
		assert.NoError(t, ValidatePMFromFileData(
			`SecRule ARGS " @pmf /etc/passwd" "id:1,phase:1,pass"`,
			nil,
		))
	})

	for _, operator := range []string{"@pmFromFile", "@pmf", "!@pmf", "!@pmFromFile"} {
		t.Run(operator+" single token with data", func(t *testing.T) {
			assert.NoError(t, ValidatePMFromFileData(
				`SecRule ARGS "`+operator+` overlap.data" "id:1,phase:1,pass"`,
				populatedData,
			))
		})

		t.Run(operator+" single token without data", func(t *testing.T) {
			require.Error(t, ValidatePMFromFileData(
				`SecRule ARGS "`+operator+` overlap.data" "id:1,phase:1,pass"`,
				nil,
			))
		})

		t.Run(operator+" multi token", func(t *testing.T) {
			err := ValidatePMFromFileData(
				`SecRule ARGS "`+operator+` overlap.data other.data" "id:1,phase:1,pass"`,
				map[string][]byte{"overlap.data": []byte("x"), "other.data": []byte("y")},
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "single data file name")
		})

		t.Run(operator+" tab-separated absolute path", func(t *testing.T) {
			err := ValidatePMFromFileData(
				"SecRule ARGS \""+operator+"\t/etc/passwd\" \"id:1,phase:1,pass\"",
				nil,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "/etc/passwd")
		})
	}
}
