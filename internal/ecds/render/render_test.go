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
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/apimachinery/pkg/types"

	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	wasmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
)

const (
	testNamespace   = "shop"
	testEngineName  = "checkout"
	testEngineUID   = types.UID("11111111-1111-4111-8111-111111111111")
	testRuleSetUID  = types.UID("22222222-2222-4222-8222-222222222222")
	testBinaryURL   = "https://coraza-operator.coraza-system.svc.cluster.local:18080/wasm/cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc.wasm"
	testBinarySHA   = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	testResourceRef = "coraza.waf.shop.checkout.11111111-1111-4111-8111-111111111111"
)

func TestJSONMarshalDeterminism(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	names := []string{"a.txt", "b.txt", "data.json", "z.txt"}

	var first string
	for i := 0; i < 100; i++ {
		values := make(map[string][]byte, len(names))
		for _, idx := range rng.Perm(len(names)) {
			values[names[idx]] = []byte("content-" + names[idx])
		}

		// The rendered payload and version hash rely on json.Marshal
		// sorting map keys; this pins that property.
		raw, err := json.Marshal(values)
		require.NoError(t, err)
		if i == 0 {
			first = string(raw)
			continue
		}
		assert.Equal(t, first, string(raw), "map insertion order must not affect the marshalled output")
	}
}

func TestRuleSetContentHash(t *testing.T) {
	base := assemble.Content{
		Rules: "SecRuleEngine On",
		DataFiles: map[string][]byte{
			"a.txt": []byte("alpha"),
			"b.txt": []byte("beta"),
		},
	}

	hash, err := render.RuleSetContentHash(base)
	require.NoError(t, err)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, hash)

	reversed := assemble.Content{
		Rules: base.Rules,
		DataFiles: map[string][]byte{
			"b.txt": []byte("beta"),
			"a.txt": []byte("alpha"),
		},
	}
	reversedHash, err := render.RuleSetContentHash(reversed)
	require.NoError(t, err)
	assert.Equal(t, hash, reversedHash, "data map insertion order must not affect the content hash")

	t.Run("whitespace changes the hash", func(t *testing.T) {
		changed := base
		changed.Rules = "SecRuleEngine  On"
		changedHash, err := render.RuleSetContentHash(changed)
		require.NoError(t, err)
		assert.NotEqual(t, hash, changedHash)
	})

	t.Run("file bytes change the hash", func(t *testing.T) {
		changed := assemble.Content{
			Rules:     base.Rules,
			DataFiles: map[string][]byte{"a.txt": []byte("alpha"), "b.txt": []byte("BETA")},
		}
		changedHash, err := render.RuleSetContentHash(changed)
		require.NoError(t, err)
		assert.NotEqual(t, hash, changedHash)
	})

	t.Run("filename changes the hash", func(t *testing.T) {
		changed := assemble.Content{
			Rules:     base.Rules,
			DataFiles: map[string][]byte{"a.txt": []byte("alpha"), "c.txt": []byte("beta")},
		}
		changedHash, err := render.RuleSetContentHash(changed)
		require.NoError(t, err)
		assert.NotEqual(t, hash, changedHash)
	})

	t.Run("nil and empty data files hash the same", func(t *testing.T) {
		nilHash, err := render.RuleSetContentHash(assemble.Content{Rules: base.Rules})
		require.NoError(t, err)
		emptyHash, err := render.RuleSetContentHash(assemble.Content{Rules: base.Rules, DataFiles: map[string][]byte{}})
		require.NoError(t, err)
		assert.Equal(t, nilHash, emptyHash)
	})
}

// TestRenderExposesResolvedInputs pins that the artifact carries the resolved
// rendering inputs, so the accepted record persists exactly what was rendered
// and never recomputes the defaults.
func TestRenderExposesResolvedInputs(t *testing.T) {
	t.Run("zero-value inputs resolve to the contract defaults", func(t *testing.T) {
		input := newEngineInput(t, assemble.Content{Rules: "SecRuleEngine On"})
		input.EmptyRulesPolicy = ""
		input.DriverType = ""

		artifact, err := render.Render(input)
		require.NoError(t, err)

		assert.Equal(t, "Reject", artifact.EmptyRulesPolicy)
		assert.Equal(t, "wasm", artifact.DriverType)
		assert.False(t, artifact.EnableFilterStateLogs)
		assert.Equal(t, render.VMSettings{
			Runtime:     "envoy.wasm.runtime.v8",
			VMID:        "coraza-" + string(testEngineUID),
			CodeTimeout: 10 * time.Second,
		}, artifact.VM)
	})

	t.Run("explicit inputs are passed through", func(t *testing.T) {
		input := newEngineInput(t, assemble.Content{Rules: "SecRuleEngine On"})
		input.EmptyRulesPolicy = "Allow"
		input.EnableFilterStateLogs = true
		input.VMSettings = render.VMSettings{
			Runtime:     "envoy.wasm.runtime.wasmtime",
			VMID:        "shared-vm",
			CodeTimeout: 5 * time.Second,
		}

		artifact, err := render.Render(input)
		require.NoError(t, err)

		assert.Equal(t, "Allow", artifact.EmptyRulesPolicy)
		assert.True(t, artifact.EnableFilterStateLogs)
		assert.Equal(t, input.VMSettings, artifact.VM)
	})
}

func TestRenderGoldenPayload(t *testing.T) {
	input := newEngineInput(t, assemble.Content{
		Rules:     "SecRuleEngine On\nSecRule ARGS \"@attack\" \"id:1001,phase:2,deny,status:403\"",
		DataFiles: map[string][]byte{"blocked.txt": []byte("attack\n")},
	})
	input.EnableFilterStateLogs = true

	artifact, err := render.Render(input)
	require.NoError(t, err)

	want := `{"schema_version":2,"engine":"checkout","namespace":"shop","engine_uid":"11111111-1111-4111-8111-111111111111","driver_type":"wasm","empty_rules_policy":"Reject","failure_policy":"fail","enable_filter_state_logs":true,"directives_map":{"default":["SecRuleEngine On\nSecRule ARGS \"@attack\" \"id:1001,phase:2,deny,status:403\""]},"default_directives":"default","rule_data":{"blocked.txt":"attack\n"}}`
	assert.Equal(t, want, string(artifact.Payload))
	assert.Equal(t, testResourceRef, artifact.ResourceName)
	// Pins the version hash so encoding changes cannot silently rehash
	// every accepted Engine.
	assert.Equal(t, "sha256:2a6695ef742ca4b2a7c4db004a327a44c4ce7893095663ad98cb7b4fefb52f70", artifact.Version)

	for _, forbidden := range []string{"resourceVersion", "nonce", "timestamp", "token", "replica"} {
		assert.NotContains(t, string(artifact.Payload), forbidden, "runtime payload must not carry non-content fields")
	}
}

func TestRenderResourceShape(t *testing.T) {
	input := newEngineInput(t, assemble.Content{Rules: "SecRuleEngine On"})

	artifact, err := render.Render(input)
	require.NoError(t, err)
	require.NotNil(t, artifact.Resource)

	assert.Equal(t, testResourceRef, artifact.Resource.Name)
	assert.Equal(t, "envoy.config.core.v3.TypedExtensionConfig", string(artifact.Resource.ProtoReflect().Descriptor().FullName()))

	var wasmFilter wasmhttp.Wasm
	require.NoError(t, artifact.Resource.TypedConfig.UnmarshalTo(&wasmFilter))
	require.NotNil(t, wasmFilter.Config)

	config := wasmFilter.Config
	assert.Equal(t, testResourceRef, config.Name)
	assert.Equal(t, testResourceRef, config.RootId)
	assert.Equal(t, wasmv3.FailurePolicy_FAIL_CLOSED, config.FailurePolicy)
	assert.True(t, config.GetAllowOnHeadersStopIteration().GetValue(),
		"envoy-coraza must be allowed to pause request headers for body inspection")

	vm := config.GetVmConfig()
	require.NotNil(t, vm)
	assert.Equal(t, "coraza-"+string(testEngineUID), vm.VmId)
	assert.Equal(t, "envoy.wasm.runtime.v8", vm.Runtime)
	assert.True(t, vm.NackOnCodeCacheMiss)

	remote := vm.GetCode().GetRemote()
	require.NotNil(t, remote)
	assert.Equal(t, testBinaryURL, remote.HttpUri.Uri)
	assert.Equal(t, "coraza_wasm", remote.HttpUri.GetCluster())
	assert.Equal(t, 10*time.Second, remote.HttpUri.Timeout.AsDuration())
	assert.Equal(t, testBinarySHA, remote.Sha256)

	var configuration wrapperspb.StringValue
	require.NoError(t, config.Configuration.UnmarshalTo(&configuration))
	assert.Equal(t, string(artifact.Payload), configuration.Value)

}

func TestRenderEmptyRulesPolicy(t *testing.T) {
	t.Run("Reject refuses an empty payload", func(t *testing.T) {
		input := newEngineInput(t, assemble.Content{})
		_, err := render.Render(input)
		require.ErrorIs(t, err, assemble.ErrEmptyRules)
	})

	t.Run("Allow renders an explicit empty payload", func(t *testing.T) {
		input := newEngineInput(t, assemble.Content{})
		input.EmptyRulesPolicy = "Allow"

		artifact, err := render.Render(input)
		require.NoError(t, err)
		assert.Contains(t, string(artifact.Payload), `"directives_map":{"default":[""]}`)
		assert.Contains(t, string(artifact.Payload), `"rule_data":{}`)
		assert.Contains(t, string(artifact.Payload), `"empty_rules_policy":"Allow"`)
	})
}

func TestEngineVersionInputs(t *testing.T) {
	baseContent := assemble.Content{Rules: "SecRuleEngine On"}
	base := newEngineInput(t, baseContent)
	baseArtifact, err := render.Render(base)
	require.NoError(t, err)

	t.Run("metadata-only input changes do not change the version", func(t *testing.T) {
		firstContent, err := assemble.Assemble(assemble.Inputs{
			Sources: []assemble.SourceInput{{Name: "rules", UID: types.UID("source-a"), Generation: 1, Rules: "SecRuleEngine On"}},
		})
		require.NoError(t, err)

		// Simulate recreated RuleSource/RuleData objects: identity changed,
		// effective content did not.
		secondContent, err := assemble.Assemble(assemble.Inputs{
			Sources: []assemble.SourceInput{{Name: "rules", UID: types.UID("source-b"), Generation: 9, Rules: "SecRuleEngine On"}},
		})
		require.NoError(t, err)

		firstArtifact, err := render.Render(newEngineInput(t, firstContent))
		require.NoError(t, err)
		secondArtifact, err := render.Render(newEngineInput(t, secondContent))
		require.NoError(t, err)

		assert.Equal(t, firstArtifact.Version, secondArtifact.Version)
		assert.Equal(t, string(firstArtifact.Payload), string(secondArtifact.Payload))
	})

	t.Run("whitespace changes the version", func(t *testing.T) {
		changed := newEngineInput(t, assemble.Content{Rules: "SecRuleEngine  On"})
		changedArtifact, err := render.Render(changed)
		require.NoError(t, err)
		assert.NotEqual(t, baseArtifact.Version, changedArtifact.Version)
	})

	tests := []struct {
		name   string
		mutate func(*render.EngineInput)
	}{
		{name: "Engine uid", mutate: func(in *render.EngineInput) { in.UID = types.UID("33333333-3333-4333-8333-333333333333") }},
		{name: "RuleSet uid", mutate: func(in *render.EngineInput) { in.RuleSet.UID = types.UID("44444444-4444-4444-8444-444444444444") }},
		{name: "failure policy", mutate: func(in *render.EngineInput) { in.FailurePolicy = "allow" }},
		{name: "observability", mutate: func(in *render.EngineInput) { in.EnableFilterStateLogs = true }},
		{name: "binary URL", mutate: func(in *render.EngineInput) { in.Binary.URL = strings.Replace(testBinaryURL, "cccc", "dddd", 1) }},
		{name: "binary digest", mutate: func(in *render.EngineInput) { in.Binary.SHA256 = strings.Repeat("d", 64) }},
		{name: "VM runtime", mutate: func(in *render.EngineInput) { in.VMSettings.Runtime = "envoy.wasm.runtime.wasmtime" }},
		{name: "VM id", mutate: func(in *render.EngineInput) { in.VMSettings.VMID = "shared-vm" }},
		{name: "code timeout", mutate: func(in *render.EngineInput) { in.VMSettings.CodeTimeout = 5 * time.Second }},
		{name: "empty rules policy", mutate: func(in *render.EngineInput) { in.EmptyRulesPolicy = "Allow" }},
	}

	for _, tt := range tests {
		t.Run(tt.name+" changes the version", func(t *testing.T) {
			changed := base
			tt.mutate(&changed)
			changedArtifact, err := render.Render(changed)
			require.NoError(t, err)
			assert.NotEqual(t, baseArtifact.Version, changedArtifact.Version)
		})
	}
}

func TestRenderRejectsInvalidInput(t *testing.T) {
	validContent := assemble.Content{Rules: "SecRuleEngine On"}

	tests := []struct {
		name    string
		mutate  func(*render.EngineInput)
		wantErr string
	}{
		{name: "missing namespace", mutate: func(in *render.EngineInput) { in.Namespace = "" }, wantErr: "engine namespace, name, and uid are required"},
		{name: "missing name", mutate: func(in *render.EngineInput) { in.Name = "" }, wantErr: "engine namespace, name, and uid are required"},
		{name: "missing uid", mutate: func(in *render.EngineInput) { in.UID = "" }, wantErr: "engine namespace, name, and uid are required"},
		{name: "missing RuleSet uid", mutate: func(in *render.EngineInput) { in.RuleSet.UID = "" }, wantErr: "RuleSet uid is required"},
		{name: "malformed content hash", mutate: func(in *render.EngineInput) { in.RuleSet.ContentHash = "sha256:xyz" }, wantErr: "must be sha256:<64 lowercase hex>"},
		{name: "content hash mismatch", mutate: func(in *render.EngineInput) { in.RuleSet.ContentHash = "sha256:" + strings.Repeat("0", 64) }, wantErr: "does not match accepted RuleSet content hash"},
		{name: "http binary URL", mutate: func(in *render.EngineInput) { in.Binary.URL = strings.Replace(testBinaryURL, "https://", "http://", 1) }, wantErr: "must be an https URL"},
		{name: "binary URL query", mutate: func(in *render.EngineInput) { in.Binary.URL += "?v=1" }, wantErr: "must be an https URL"},
		{name: "binary URL userinfo", mutate: func(in *render.EngineInput) { in.Binary.URL = "https://user:secret@example.com/plugin.wasm" }, wantErr: "without userinfo, query, or fragment"},
		{name: "uppercase digest", mutate: func(in *render.EngineInput) { in.Binary.SHA256 = strings.Repeat("C", 64) }, wantErr: "must be 64 lowercase hex characters"},
		{name: "short digest", mutate: func(in *render.EngineInput) { in.Binary.SHA256 = "abc" }, wantErr: "must be 64 lowercase hex characters"},
		{name: "unknown driver", mutate: func(in *render.EngineInput) { in.DriverType = "sidecar" }, wantErr: `unsupported driver type "sidecar"`},
		{name: "unknown failure policy", mutate: func(in *render.EngineInput) { in.FailurePolicy = "closed" }, wantErr: `unsupported failure policy "closed"`},
		{name: "unknown empty rules policy", mutate: func(in *render.EngineInput) { in.EmptyRulesPolicy = "reject" }, wantErr: `unsupported empty rules policy "reject"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := newEngineInput(t, validContent)
			tt.mutate(&input)
			_, err := render.Render(input)
			require.ErrorContains(t, err, tt.wantErr)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

// TestRenderContentHashMismatchWrapsAcceptedMismatch pins that a content
// divergence carries the ErrAcceptedMismatch sentinel: it is the signal
// publication uses to count coraza_xds_hash_mismatch_total.
func TestRenderContentHashMismatchWrapsAcceptedMismatch(t *testing.T) {
	input := newEngineInput(t, assemble.Content{Rules: "SecRuleEngine On"})
	input.RuleSet.ContentHash = "sha256:" + strings.Repeat("0", 64)
	_, err := render.Render(input)
	require.ErrorIs(t, err, render.ErrAcceptedMismatch)
}

func TestRenderDataFiles(t *testing.T) {
	files := map[string][]byte{
		"some.data": []byte("evildata\nanotherevildata\n"),
		"aaa.data":  []byte("alpha"),
	}
	input := newEngineInput(t, assemble.Content{
		Rules:     "SecRule ARGS \"@pmFromFile some.data\" \"id:3002,phase:2,deny,status:403\"",
		DataFiles: files,
	})

	artifact, err := render.Render(input)
	require.NoError(t, err)

	// Canonical JSON sorts keys, so rule_data order is deterministic.
	assert.Contains(t, string(artifact.Payload),
		`"rule_data":{"aaa.data":"alpha","some.data":"evildata\nanotherevildata\n"}`)
	assert.Contains(t, string(artifact.Payload), `"directives_map":{"default":["SecRule ARGS \"@pmFromFile some.data\"`)

	// The runtime payload must carry the schema version this build renders;
	// publication no longer re-decodes the payload to check it.
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	require.NoError(t, json.Unmarshal(artifact.Payload, &header))
	assert.Equal(t, render.SchemaVersion, header.SchemaVersion)

	// RuleData is delivered in the payload only; the module stays remote and
	// digest-pinned.
	var wasmFilter wasmhttp.Wasm
	require.NoError(t, artifact.Resource.TypedConfig.UnmarshalTo(&wasmFilter))
	remote := wasmFilter.Config.GetVmConfig().GetCode().GetRemote()
	require.NotNil(t, remote)
	assert.Equal(t, testBinaryURL, remote.HttpUri.Uri)
	assert.Equal(t, testBinarySHA, remote.Sha256)

	changed := newEngineInput(t, assemble.Content{
		Rules:     input.Content.Rules,
		DataFiles: map[string][]byte{"some.data": []byte("other\n")},
	})
	changedArtifact, err := render.Render(changed)
	require.NoError(t, err)
	assert.NotEqual(t, artifact.Version, changedArtifact.Version)
}

func TestMatches(t *testing.T) {
	input := newEngineInput(t, assemble.Content{Rules: "SecRuleEngine On"})
	artifact, err := render.Render(input)
	require.NoError(t, err)

	accepted := func() render.AcceptedEngine {
		return render.AcceptedEngine{
			RenderingSchemaVersion: render.RenderingSchemaVersion,
			Version:                artifact.Version,
			RuleSetUID:             string(input.RuleSet.UID),
			RuleSetContentHash:     input.RuleSet.ContentHash,
			BinaryURL:              input.Binary.URL,
			WasmSHA256:             input.Binary.SHA256,
			FailurePolicy:          "fail",
		}
	}

	require.NoError(t, render.Matches(accepted(), artifact))

	tests := []struct {
		name   string
		field  string
		mutate func(*render.AcceptedEngine)
	}{
		{name: "rendering schema version", field: "rendering schema version", mutate: func(a *render.AcceptedEngine) { a.RenderingSchemaVersion = render.RenderingSchemaVersion + 1 }},
		{name: "version", field: "version", mutate: func(a *render.AcceptedEngine) { a.Version = "sha256:" + strings.Repeat("0", 64) }},
		{name: "RuleSet uid", field: "RuleSet uid", mutate: func(a *render.AcceptedEngine) { a.RuleSetUID = "other" }},
		{name: "RuleSet content hash", field: "RuleSet content hash", mutate: func(a *render.AcceptedEngine) { a.RuleSetContentHash = "sha256:" + strings.Repeat("0", 64) }},
		{name: "binary URL", field: "binary URL", mutate: func(a *render.AcceptedEngine) { a.BinaryURL = "https://example.com/other.wasm" }},
		{name: "WASM digest", field: "WASM sha256", mutate: func(a *render.AcceptedEngine) { a.WasmSHA256 = strings.Repeat("0", 64) }},
		{name: "failure policy", field: "failure policy", mutate: func(a *render.AcceptedEngine) { a.FailurePolicy = "allow" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mismatched := accepted()
			tt.mutate(&mismatched)
			err := render.Matches(mismatched, artifact)
			require.ErrorIs(t, err, render.ErrAcceptedMismatch)
			assert.ErrorContains(t, err, tt.field)
		})
	}

	// Pin the exact diagnostic shapes: %d for the schema version, %q for
	// strings, in row order.
	t.Run("diagnostics", func(t *testing.T) {
		mismatched := accepted()
		mismatched.RenderingSchemaVersion = render.RenderingSchemaVersion + 1
		mismatched.Version = "sha256:" + strings.Repeat("0", 64)
		mismatched.FailurePolicy = "allow"

		err := render.Matches(mismatched, artifact)
		require.ErrorIs(t, err, render.ErrAcceptedMismatch)
		assert.Contains(t, err.Error(), fmt.Sprintf("rendering schema version: accepted %d, rendered %d", render.RenderingSchemaVersion+1, render.RenderingSchemaVersion))
		assert.Contains(t, err.Error(), fmt.Sprintf("version: accepted %q, rendered %q", mismatched.Version, artifact.Version))
		assert.Contains(t, err.Error(), `failure policy: accepted "allow", rendered "fail"`)
	})
}

func TestRenderSizeEnvelope(t *testing.T) {
	// shortcut: synthetic CRS-sized rules stand in for the third-party CRS
	// fixture; replace with the pinned CRS when a checked-in fixture exists.
	const maxECDSMessageBytes = 16 << 20 // planned --xds-max-message-bytes default

	var rules strings.Builder
	rules.WriteString("SecRuleEngine On\n")
	for i := 0; rules.Len() < 1<<20; i++ {
		fmt.Fprintf(&rules, "SecRule REQUEST_HEADERS:X-Test-%d \"@rx ^[a-z0-9]+$\" \"id:%d,phase:2,pass,nolog,msg:'CRS-sized fixture line %d'\"\n", i, 100000+i, i)
	}

	input := newEngineInput(t, assemble.Content{Rules: rules.String()})

	artifact, err := render.Render(input)
	require.NoError(t, err)
	assert.Less(t, len(artifact.Payload), maxECDSMessageBytes, "runtime payload must fit the message limit")

	wire, err := proto.Marshal(artifact.Resource)
	require.NoError(t, err)
	assert.Less(t, len(wire), maxECDSMessageBytes, "encoded ECDS resource must fit the message limit")
}

func newEngineInput(t *testing.T, content assemble.Content) render.EngineInput {
	t.Helper()

	contentHash, err := render.RuleSetContentHash(content)
	require.NoError(t, err)

	return render.EngineInput{
		Namespace: testNamespace,
		Name:      testEngineName,
		UID:       testEngineUID,
		RuleSet: render.RuleSetAccepted{
			UID:         testRuleSetUID,
			ContentHash: contentHash,
		},
		Content:          content,
		EmptyRulesPolicy: "Reject",
		FailurePolicy:    "fail",
		DriverType:       "wasm",
		Binary: render.BinaryRef{
			URL:    testBinaryURL,
			SHA256: testBinarySHA,
		},
	}
}

// envoy-coraza has no embedded alias filesystem or glob expansion; a payload
// that used either would NACK at configure time while the Engine looked Ready.
func TestRenderRejectsUnsupportedIncludes(t *testing.T) {
	for _, rules := range []string{
		"Include @owasp_crs/*.conf",
		"Include @recommended-conf",
		"Include rules/*.conf",
	} {
		_, err := render.Render(newEngineInput(t, assemble.Content{Rules: rules}))
		require.Error(t, err, rules)
	}

	// coraza-rs's native embedded-CRS namespace is supported by a module
	// built with embed-crs.
	orig := validation.BundledCRSVersion
	validation.BundledCRSVersion = "v4.30.0"
	t.Cleanup(func() { validation.BundledCRSVersion = orig })
	_, err := render.Render(newEngineInput(t, assemble.Content{
		Rules: "SecRuleEngine On\nInclude crs/crs-setup.conf\nInclude crs/rules/*.conf",
	}))
	require.NoError(t, err)
}

// A module without embedded CRS resolves crs/ includes to nothing: the
// payload would compile a zero-rule WAF while the Engine looked Ready, so
// rendering rejects it (the committed fixture manifest records none).
func TestRenderRejectsCRSWithoutBundledCRS(t *testing.T) {
	_, err := render.Render(newEngineInput(t, assemble.Content{
		Rules: "SecRuleEngine On\nInclude crs/crs-setup.conf\nInclude crs/rules/*.conf",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not include OWASP CRS")
}
