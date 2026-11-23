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

package render_test

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

func TestEncodedLenMatchesJSON(t *testing.T) {
	var ascii strings.Builder
	for b := range 0x80 {
		ascii.WriteByte(byte(b))
	}
	for _, s := range []string{
		"",
		ascii.String(),
		`SecRule ARGS "@rx <script>" "id:1,msg:'a&b'" \` + "\n\t\r\b\f",
		"é日本😀",
		"\u2028\u2029",
	} {
		doc, err := json.Marshal(s)
		require.NoError(t, err)
		assert.Equal(t, len(doc)-2, render.EncodedLen(s), "%q", s)
	}
	for _, s := range []string{"\xff", "a\xfe\xffb"} {
		doc, err := json.Marshal(s)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, render.EncodedLen(s), len(doc)-2, "invalid UTF-8 must not be under-counted: %q", s)
	}
}

// TestContentBudgetFitsTheLongestEngine renders rule text that encodes to
// exactly ContentBudget for an Engine with the longest name and namespace: it
// fills MaxConfigurationBytes, and one more byte is refused.
func TestContentBudgetFitsTheLongestEngine(t *testing.T) {
	renderRules := func(rules string) error {
		_, err := render.Render(longestEngineInput(t, assemble.Content{Rules: rules}))
		return err
	}
	require.NoError(t, renderRules(strings.Repeat("#", render.ContentBudget)))
	require.ErrorContains(t, renderRules(strings.Repeat("#", render.ContentBudget+1)), "exceeds the plugin configuration limit")
}

// TestEnvelopeLen checks that high-entropy text that encodes within the
// budget still compresses past the plugin's limit.
func TestEnvelopeLen(t *testing.T) {
	const alphabet = "!#$%'()*+,-./0123456789:;=?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[]^_abcdefghijklmnopqrstuvwxyz{|}~"
	entropy := make([]byte, render.ContentBudget)
	for i := range entropy {
		entropy[i] = alphabet[rand.Intn(len(alphabet))]
	}

	n, err := render.EnvelopeLen(assemble.Content{Rules: string(entropy)})
	require.NoError(t, err)
	assert.Greater(t, n, render.MaxConfigurationBytes, "high-entropy text must not fit once compressed")
	t.Logf("%d bytes of high-entropy text compress to a %d-byte envelope", len(entropy), n)

	n, err = render.EnvelopeLen(assemble.Content{Rules: string(entropy[:4<<20])})
	require.NoError(t, err)
	assert.LessOrEqual(t, n, render.MaxConfigurationBytes, "4 MiB cannot compress past the limit")
}

// longestEngineInput renders content for an Engine with the longest name,
// namespace and policy values.
func longestEngineInput(t *testing.T, content assemble.Content) render.EngineInput {
	t.Helper()
	input := newEngineInput(t, content)
	input.Name = strings.Repeat("e", 253)
	input.Namespace = strings.Repeat("n", 63)
	input.UID = types.UID(strings.Repeat("u", 36))
	input.FailurePolicy = "allow"
	return input
}
