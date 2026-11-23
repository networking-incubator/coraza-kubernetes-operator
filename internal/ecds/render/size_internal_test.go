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

package render

import (
	"crypto/rand"
	"encoding/base64"
	"math"
	mathrand "math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

// TestMaxEnvelopeLenBoundsIncompressibleData wraps random bytes, which deflate
// cannot compress, and checks the bound EnvelopeLen relies on.
func TestMaxEnvelopeLenBoundsIncompressibleData(t *testing.T) {
	for _, n := range []int{0, 1, 16 << 10, 1<<20 + 7, 6 << 20} {
		data := make([]byte, n)
		_, err := rand.Read(data)
		require.NoError(t, err)
		envelope, err := gzipB64Envelope(data)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(envelope), maxEnvelopeLen(n), "n=%d", n)
	}
}

// TestEnvelopeLenMatchesRender renders content that needs compressing for the
// Engine EnvelopeLen models: EnvelopeLen is the envelope Render produces plus
// the headroom for another Engine's identity.
func TestEnvelopeLenMatchesRender(t *testing.T) {
	content := assemble.Content{Rules: strings.Repeat("#", ContentBudget)}
	contentHash, err := RuleSetContentHash(content)
	require.NoError(t, err)
	n, err := EnvelopeLen(content)
	require.NoError(t, err)
	artifact, err := Render(EngineInput{
		Namespace:        longestEngineNamespace,
		Name:             longestEngineName,
		UID:              types.UID(longestEngineUID),
		RuleSet:          RuleSetAccepted{UID: "ruleset", ContentHash: contentHash},
		Content:          content,
		EmptyRulesPolicy: string(assemble.EmptyRulesPolicyReject),
		FailurePolicy:    "allow",
		Binary:           BinaryRef{URL: "https://example.com/module.wasm", SHA256: strings.Repeat("c", 64)},
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(artifact.Payload), n)
	assert.LessOrEqual(t, n-len(artifact.Payload), base64.StdEncoding.EncodedLen(identityHeadroom)+4)
}

// TestEnvelopeLenBoundsEveryEngine fills content up to the most EnvelopeLen
// accepts and compresses it for Engines with random names, namespaces and
// UIDs of the longest lengths: each envelope fits the limit. One content
// starts with a copy of the modelled identity, which only the model can
// compress into a back-reference, followed by poorly compressible text that
// shares no characters with an Engine name.
func TestEnvelopeLenBoundsEveryEngine(t *testing.T) {
	random := mathrand.New(mathrand.NewSource(5))
	draw := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[random.Intn(len(alphabet))]
		}
		return string(b)
	}
	const label, hex = "abcdefghijklmnopqrstuvwxyz0123456789", "0123456789abcdef"
	const symbols = "ABCDEFGHIJKLMNOPQRSTUVWXYZ!#$%()*+,/:;=?@[]^_`{|}~"
	var adversarial strings.Builder
	adversarial.WriteString(longestEngineName + " " + longestEngineNamespace + " " + longestEngineUID + " ")
	for adversarial.Len() < ContentBudget+utf8.UTFMax {
		if random.Intn(2) == 0 {
			adversarial.WriteByte(symbols[random.Intn(len(symbols))])
		} else {
			adversarial.WriteRune(rune(0x80 + random.Intn(0x780))) // two bytes in UTF-8
		}
	}
	shapes := map[string]string{
		"random base64-alphabet text":                draw("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", ContentBudget+1),
		"a copy of the modelled identity, then text": adversarial.String(),
	}
	worst := math.MinInt
	for name, rules := range shapes {
		// Cut the content back to just within what EnvelopeLen accepts.
		content := assemble.Content{}
		for size, attempt := ContentBudget, 0; ; attempt++ {
			require.Less(t, attempt, 20, "%s did not settle close to the limit", name)
			for !utf8.RuneStart(rules[size]) {
				size--
			}
			content.Rules = rules[:size]
			n, err := EnvelopeLen(content)
			require.NoError(t, err)
			if n <= MaxConfigurationBytes && n > MaxConfigurationBytes-1024 {
				break
			}
			size = min(ContentBudget, int(int64(size)*int64(MaxConfigurationBytes-256)/int64(n)))
		}
		for range 10 {
			uid := draw(hex, 8) + "-" + draw(hex, 4) + "-" + draw(hex, 4) + "-" + draw(hex, 4) + "-" + draw(hex, 12)
			plain := mustMarshal(runtimeDocument(content, draw(label, 253), draw(label, 63), uid,
				defaultDriverType, string(assemble.EmptyRulesPolicyReject), "allow", false))
			var compressed byteCounter
			require.NoError(t, gzipTo(&compressed, plain))
			envelope := envelopeLen(int(compressed))
			assert.LessOrEqual(t, envelope, MaxConfigurationBytes, "%s: a random Engine's envelope must fit", name)
			worst = max(worst, envelope-MaxConfigurationBytes)
		}
	}
	t.Logf("closest random Engine envelope to the limit: %d bytes", worst)
}
