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
	"encoding/base64"
	"encoding/json"
	"math/rand/v2"
	"unicode/utf8"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

var (
	// emptyDocumentLen is the size of a runtime configuration with empty rule
	// text and no rule data, for an Engine with the longest name, namespace
	// and policy values.
	emptyDocumentLen = len(mustMarshal(longestEngineDocument(assemble.Content{})))

	// ContentBudget is the most bytes the JSON-encoded rule text and rule
	// data may take in one runtime configuration. Content within it renders
	// within MaxConfigurationBytes for every Engine.
	ContentBudget = MaxConfigurationBytes - emptyDocumentLen
)

// The longest Engine name, namespace and UID, drawn from a fixed seed so every
// replica computes the same value: text without repetition compresses least.
var longestEngineName, longestEngineNamespace, longestEngineUID = func() (string, string, string) {
	random := rand.New(rand.NewPCG(1, 1)) //nolint:gosec // a fixed seed: every replica draws the same identity
	draw := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[random.IntN(len(alphabet))]
		}
		return string(b)
	}
	const label, hex = "abcdefghijklmnopqrstuvwxyz0123456789", "0123456789abcdef"
	return draw(label, k8svalidation.DNS1123SubdomainMaxLength),
		draw(label, k8svalidation.DNS1123LabelMaxLength),
		draw(hex, 8) + "-" + draw(hex, 4) + "-" + draw(hex, 4) + "-" + draw(hex, 4) + "-" + draw(hex, 12)
}()

// longestEngineDocument is the runtime configuration of content for an Engine
// with the longest name, namespace and policy values.
func longestEngineDocument(content assemble.Content) runtimeConfig {
	return runtimeDocument(content,
		longestEngineName,
		longestEngineNamespace,
		longestEngineUID,
		defaultDriverType,
		string(assemble.EmptyRulesPolicyReject),
		"allow", // longer than "fail"
		false)
}

func mustMarshal(doc runtimeConfig) []byte {
	plain, err := json.Marshal(doc)
	if err != nil {
		panic("render: marshal runtime configuration: " + err.Error())
	}
	return plain
}

// DataEntryLen is the size one rule data file takes in the runtime
// configuration, without the comma between entries.
func DataEntryLen(name, data string) int {
	return EncodedLen(name) + EncodedLen(data) + len(`"":""`)
}

// EnvelopeLen returns the size of the config_gzip_b64 envelope Render wraps
// content in, for the longest Engine identity. The plugin caps the envelope at
// MaxConfigurationBytes too, and content that encodes within ContentBudget can
// still compress too poorly to fit. Below the size at which that can happen it
// returns an upper bound instead of compressing.
func EnvelopeLen(content assemble.Content) (int, error) {
	n := emptyDocumentLen + EncodedLen(content.Rules)
	for name, data := range content.DataFiles {
		n += DataEntryLen(name, string(data)) + 1
	}
	if bound := maxEnvelopeLen(n); bound <= MaxConfigurationBytes {
		return bound, nil
	}
	var compressed byteCounter
	if err := gzipTo(&compressed, mustMarshal(longestEngineDocument(content))); err != nil {
		return 0, err
	}
	return envelopeLen(int(compressed) + identityHeadroom), nil
}

// identityHeadroom bounds how many more compressed bytes the document of
// another Engine can take than the modelled one. Content can hold a copy of
// the modelled name, namespace and UID, which the model compresses into one
// back-reference while another Engine pays that copy and its own identity as
// literals, of at most 15 bits each in deflate.
var identityHeadroom = (2*(len(longestEngineName)+len(longestEngineNamespace)+len(longestEngineUID))*15 + 7) / 8

// maxEnvelopeLen bounds the envelope of an n-byte document whatever it
// compresses to: deflate stores a block it cannot compress, with 5 bytes of
// framing per block of at least 16 KiB, and gzip adds 18 bytes.
func maxEnvelopeLen(n int) int {
	return envelopeLen(n + 5*(n/(16<<10)+1) + 18)
}

// envelopeLen is the size of the envelope around compressed bytes of gzip.
func envelopeLen(compressed int) int {
	return len(`{"config_gzip_b64":""}`) + base64.StdEncoding.EncodedLen(compressed)
}

// byteCounter is an io.Writer that only counts what is written to it.
type byteCounter int

func (c *byteCounter) Write(p []byte) (int, error) {
	*c += byteCounter(len(p))
	return len(p), nil
}

// encodedByteLen is how many bytes each ASCII byte takes inside a JSON string:
// encoding/json escapes quotes, backslashes and control characters, and <, >
// and & for HTML.
var encodedByteLen = func() (lengths [utf8.RuneSelf]uint8) {
	for b := range lengths {
		switch {
		case b == '"', b == '\\', b == '\b', b == '\f', b == '\n', b == '\r', b == '\t':
			lengths[b] = 2
		case b < 0x20, b == '<', b == '>', b == '&':
			lengths[b] = 6
		default:
			lengths[b] = 1
		}
	}
	return lengths
}()

// EncodedLen returns how many bytes s takes inside a JSON string as Render
// encodes it, without the quotes. Beyond ASCII, encoding/json escapes U+2028
// and U+2029; an invalid UTF-8 byte counts as the longest form it can take.
func EncodedLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			n += int(encodedByteLen[b])
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1, r == '\u2028', r == '\u2029':
			n += 6
		default:
			n += size
		}
		i += size
	}
	return n
}
