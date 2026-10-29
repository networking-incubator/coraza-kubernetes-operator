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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLimitsWithDefaultsKeepsPartiallySetLimits checks that a caller that sets
// one field does not silently get zero limits on the others.
func TestLimitsWithDefaultsKeepsPartiallySetLimits(t *testing.T) {
	filled := Limits{MaxMessageBytes: 1024}.WithDefaults()
	assert.Equal(t, 1024, filled.MaxMessageBytes, "an explicit value must win")
	assert.Equal(t, DefaultMaxDataFiles, filled.MaxDataFiles, "an unset limit must default")
	assert.Equal(t, DefaultMaxDecodedDataBytes, filled.MaxDecodedDataBytes)
	assert.Equal(t, DefaultStreamMaxLifetime, filled.StreamMaxLifetime)

	assert.Equal(t, DefaultLimits().MaxDataFiles, Limits{}.WithDefaults().MaxDataFiles, "zero limits must default")
}

// TestLimitsWithDefaultsRejectsNegativeLimits checks that a negative limit
// means "use the default" like zero does, and that retries — where zero is
// meaningful — only clamp negatives.
func TestLimitsWithDefaultsRejectsNegativeLimits(t *testing.T) {
	filled := Limits{
		MaxStreams:         -DefaultMaxStreams,
		StreamMaxLifetime:  -time.Second,
		TokenReviewRetries: -3,
	}.WithDefaults()
	assert.Equal(t, DefaultMaxStreams, filled.MaxStreams, "a negative limit must fall back to the default")
	assert.Equal(t, DefaultStreamMaxLifetime, filled.StreamMaxLifetime)
	assert.Equal(t, 0, filled.TokenReviewRetries, "negative retries must clamp to zero, not the default")
	assert.Equal(t, 0, Limits{}.WithDefaults().TokenReviewRetries, "zero retries must stay zero; only a set value or DefaultLimits applies")
}

// TestLimitsValidateRejectsPluginCapViolations checks that the operator refuses
// limits the plugin would NACK, which would leave a Ready Engine unpublished.
func TestLimitsValidateRejectsPluginCapViolations(t *testing.T) {
	assert.NoError(t, DefaultLimits().Validate())
	assert.NoError(t, Limits{}.Validate(), "unset limits must not fail validation")

	err := Limits{MaxMessageBytes: DefaultMaxMessageBytes + 1}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xds-max-message-bytes")

	err = Limits{MaxDecodedDataBytes: pluginMaxDecodedDataBytes + 1}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xds-max-decoded-data-bytes")

	err = Limits{MaxDataFiles: pluginMaxDataFiles + 1}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xds-max-data-files")
}

// TestLimitsValidateRejectsUndeliverableCombination checks the combined bound:
// the decoded rule_data plus the maximum key overhead must fit one message
// after the envelope, or accepted Engines would never be delivered.
func TestLimitsValidateRejectsUndeliverableCombination(t *testing.T) {
	for _, files := range []int{1, 3, 1024} {
		decoded := files
		maxMessage := decoded + files*(DataFileNameBytes+dataFileKeyOverheadBytes) + MessageEnvelopeBytes

		assert.NoError(t, Limits{
			MaxMessageBytes:     maxMessage,
			MaxDecodedDataBytes: decoded,
			MaxDataFiles:        files,
		}.Validate(), "the boundary must be accepted")

		err := Limits{
			MaxMessageBytes:     maxMessage - 1,
			MaxDecodedDataBytes: decoded,
			MaxDataFiles:        files,
		}.Validate()
		require.Error(t, err, "the keyed rule_data must count against the message limit")
		assert.Contains(t, err.Error(), "xds-max-message-bytes")
	}
}
