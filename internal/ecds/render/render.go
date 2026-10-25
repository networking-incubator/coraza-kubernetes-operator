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

// Package render turns assembled RuleSet content into the exact ECDS
// TypedExtensionConfig served to Envoy and hashes its effective configuration.
// It performs no Kubernetes I/O and emits no timestamps, nonces, replica
// identity, credentials, or CA material.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	wasmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

const (
	// SchemaVersion is the runtime configuration schema version.
	SchemaVersion = 1
	// RenderingSchemaVersion is the rendering schema version. It is part of
	// every engine version hash.
	RenderingSchemaVersion = 1

	defaultDriverType    = "wasm"
	defaultFailurePolicy = "fail"
	defaultRuntime       = "envoy.wasm.runtime.v8"
	defaultCodeTimeout   = 10 * time.Second
	// wasmCluster is the Envoy cluster that serves the WASM binary. The
	// cluster is installed by the EnvoyFilter in a later phase.
	wasmCluster = "coraza_wasm"
)

var (
	sha256HexPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sha256VersionPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ErrAcceptedMismatch is wrapped by Matches when a rendered artifact does not
// match the Engine's accepted configuration.
var ErrAcceptedMismatch = errors.New("rendered artifact does not match accepted configuration")

// RuleSetAccepted identifies the accepted RuleSet content used for rendering.
type RuleSetAccepted struct {
	UID         types.UID
	ContentHash string // "sha256:<64hex>"
}

// BinaryRef is the raw WASM binary location and digest.
type BinaryRef struct {
	URL    string
	SHA256 string // 64 lowercase hex
}

// VMSettings controls the WASM VM configuration. Zero values select the
// contract defaults.
type VMSettings struct {
	Runtime     string        // defaults to envoy.wasm.runtime.v8
	VMID        string        // defaults to "coraza-<engine-uid>"
	CodeTimeout time.Duration // defaults to 10s
}

// EngineInput is the complete effective Engine configuration for one render.
type EngineInput struct {
	Namespace             string
	Name                  string
	UID                   types.UID
	RuleSet               RuleSetAccepted
	Content               assemble.Content // required to render the runtime payload
	EmptyRulesPolicy      string           // "Reject" | "Allow"
	FailurePolicy         string           // "fail" | "allow"
	EnableFilterStateLogs bool
	DriverType            string
	Binary                BinaryRef
	VMSettings            VMSettings
}

// AcceptedEngine is the Engine's accepted configuration record that an
// artifact must reproduce before it may be published.
type AcceptedEngine struct {
	RenderingSchemaVersion int32
	Version                string // "sha256:<64hex>"
	RuleSetUID             string
	RuleSetContentHash     string
	BinaryURL              string
	WasmSHA256             string
	FailurePolicy          string
}

// Artifact is one rendered ECDS resource.
type Artifact struct {
	ResourceName string
	Version      string // "sha256:<64hex>"
	Payload      []byte // runtime configuration JSON, canonical
	Resource     *corev3.TypedExtensionConfig

	// identity supports Matches without adding data to the wire payload.
	identity artifactIdentity
}

type artifactIdentity struct {
	renderingSchemaVersion int32
	ruleSetUID             string
	ruleSetContentHash     string
	binaryURL              string
	wasmSHA256             string
	failurePolicy          string
}

// RuleSetContentHash returns the content hash of assembled rules and data.
// Filenames are sorted and bytes are hashed unchanged, so map insertion order
// does not affect the result.
func RuleSetContentHash(content assemble.Content) (string, error) {
	names := make([]string, 0, len(content.DataFiles))
	for name := range content.DataFiles {
		names = append(names, name)
	}
	sort.Strings(names)

	type dataFile struct {
		Name    string `json:"name"`
		Content []byte `json:"content"`
	}
	files := make([]dataFile, 0, len(names))
	for _, name := range names {
		data := content.DataFiles[name]
		if data == nil {
			data = []byte{}
		}
		files = append(files, dataFile{Name: name, Content: data})
	}

	raw, err := CanonicalJSON(struct {
		Rules     string     `json:"rules"`
		DataFiles []dataFile `json:"data_files"`
	}{
		Rules:     content.Rules,
		DataFiles: files,
	})
	if err != nil {
		return "", fmt.Errorf("hash RuleSet content: %w", err)
	}

	return digest(raw), nil
}

// Render produces the complete ECDS resource for one Engine.
func Render(in EngineInput) (Artifact, error) {
	if in.Namespace == "" || in.Name == "" || in.UID == "" {
		return Artifact{}, errors.New("engine namespace, name, and uid are required")
	}
	if in.RuleSet.UID == "" {
		return Artifact{}, errors.New("RuleSet uid is required")
	}
	if !sha256VersionPattern.MatchString(in.RuleSet.ContentHash) {
		return Artifact{}, fmt.Errorf("RuleSet content hash %q must be sha256:<64 lowercase hex>", in.RuleSet.ContentHash)
	}
	if err := validateBinaryRef(in.Binary); err != nil {
		return Artifact{}, err
	}

	driverType, err := resolveDriverType(in.DriverType)
	if err != nil {
		return Artifact{}, err
	}
	failurePolicy, wasmFailurePolicy, err := resolveFailurePolicy(in.FailurePolicy)
	if err != nil {
		return Artifact{}, err
	}
	emptyRulesPolicy, err := resolveEmptyRulesPolicy(in.EmptyRulesPolicy)
	if err != nil {
		return Artifact{}, err
	}

	contentHash, err := RuleSetContentHash(in.Content)
	if err != nil {
		return Artifact{}, err
	}
	if contentHash != in.RuleSet.ContentHash {
		return Artifact{}, fmt.Errorf("assembled content hash %s does not match accepted RuleSet content hash %s", contentHash, in.RuleSet.ContentHash)
	}
	if emptyRulesPolicy == string(assemble.EmptyRulesPolicyReject) && strings.TrimSpace(in.Content.Rules) == "" {
		return Artifact{}, fmt.Errorf("%w: Engine %s/%s has empty rules and empty rules policy is Reject", assemble.ErrEmptyRules, in.Namespace, in.Name)
	}

	runtime, vmID, codeTimeout := resolveVMSettings(in.VMSettings, in.UID)
	resourceName := resourceName(in.Namespace, in.Name, in.UID)

	dataFiles := make(map[string][]byte, len(in.Content.DataFiles))
	for name, data := range in.Content.DataFiles {
		if data == nil {
			data = []byte{}
		}
		dataFiles[name] = data
	}
	payload, err := CanonicalJSON(runtimeConfig{
		SchemaVersion:         SchemaVersion,
		Engine:                in.Name,
		Namespace:             in.Namespace,
		EngineUID:             string(in.UID),
		DriverType:            driverType,
		EmptyRulesPolicy:      emptyRulesPolicy,
		FailurePolicy:         failurePolicy,
		EnableFilterStateLogs: in.EnableFilterStateLogs,
		Rules:                 in.Content.Rules,
		DataFiles:             dataFiles,
	})
	if err != nil {
		return Artifact{}, fmt.Errorf("render runtime configuration: %w", err)
	}

	version, err := engineVersion(in, driverType, emptyRulesPolicy, failurePolicy, runtime, vmID, codeTimeout)
	if err != nil {
		return Artifact{}, err
	}

	resource, err := buildResource(resourceName, in.Binary, runtime, vmID, codeTimeout, wasmFailurePolicy, payload)
	if err != nil {
		return Artifact{}, err
	}

	return Artifact{
		ResourceName: resourceName,
		Version:      version,
		Payload:      payload,
		Resource:     resource,
		identity: artifactIdentity{
			renderingSchemaVersion: RenderingSchemaVersion,
			ruleSetUID:             string(in.RuleSet.UID),
			ruleSetContentHash:     in.RuleSet.ContentHash,
			binaryURL:              in.Binary.URL,
			wasmSHA256:             in.Binary.SHA256,
			failurePolicy:          failurePolicy,
		},
	}, nil
}

// Matches verifies that a rendered artifact reproduces the Engine's accepted
// configuration. A replica must not publish on a mismatch.
func Matches(accepted AcceptedEngine, a Artifact) error {
	var mismatches []error

	if accepted.RenderingSchemaVersion != a.identity.renderingSchemaVersion {
		mismatches = append(mismatches, fmt.Errorf("rendering schema version: accepted %d, rendered %d", accepted.RenderingSchemaVersion, a.identity.renderingSchemaVersion))
	}
	if accepted.Version != a.Version {
		mismatches = append(mismatches, fmt.Errorf("version: accepted %q, rendered %q", accepted.Version, a.Version))
	}
	if accepted.RuleSetUID != a.identity.ruleSetUID {
		mismatches = append(mismatches, fmt.Errorf("RuleSet uid: accepted %q, rendered %q", accepted.RuleSetUID, a.identity.ruleSetUID))
	}
	if accepted.RuleSetContentHash != a.identity.ruleSetContentHash {
		mismatches = append(mismatches, fmt.Errorf("RuleSet content hash: accepted %q, rendered %q", accepted.RuleSetContentHash, a.identity.ruleSetContentHash))
	}
	if accepted.BinaryURL != a.identity.binaryURL {
		mismatches = append(mismatches, fmt.Errorf("binary URL: accepted %q, rendered %q", accepted.BinaryURL, a.identity.binaryURL))
	}
	if accepted.WasmSHA256 != a.identity.wasmSHA256 {
		mismatches = append(mismatches, fmt.Errorf("WASM sha256: accepted %q, rendered %q", accepted.WasmSHA256, a.identity.wasmSHA256))
	}
	if accepted.FailurePolicy != a.identity.failurePolicy {
		mismatches = append(mismatches, fmt.Errorf("failure policy: accepted %q, rendered %q", accepted.FailurePolicy, a.identity.failurePolicy))
	}

	if len(mismatches) > 0 {
		return fmt.Errorf("%w: %w", ErrAcceptedMismatch, errors.Join(mismatches...))
	}
	return nil
}

// runtimeConfig is the schema_version 1 JSON passed to the plugin.
type runtimeConfig struct {
	SchemaVersion         int               `json:"schema_version"`
	Engine                string            `json:"engine"`
	Namespace             string            `json:"namespace"`
	EngineUID             string            `json:"engine_uid"`
	DriverType            string            `json:"driver_type"`
	EmptyRulesPolicy      string            `json:"empty_rules_policy"`
	FailurePolicy         string            `json:"failure_policy"`
	EnableFilterStateLogs bool              `json:"enable_filter_state_logs"`
	Rules                 string            `json:"rules"`
	DataFiles             map[string][]byte `json:"data_files"`
}

type vmVersion struct {
	Runtime     string `json:"runtime"`
	VMID        string `json:"vm_id"`
	CodeTimeout string `json:"code_timeout"`
}

type engineVersionInput struct {
	SchemaVersion          int       `json:"schema_version"`
	RenderingSchemaVersion int       `json:"rendering_schema_version"`
	Engine                 string    `json:"engine"`
	Namespace              string    `json:"namespace"`
	EngineUID              string    `json:"engine_uid"`
	DriverType             string    `json:"driver_type"`
	EmptyRulesPolicy       string    `json:"empty_rules_policy"`
	FailurePolicy          string    `json:"failure_policy"`
	EnableFilterStateLogs  bool      `json:"enable_filter_state_logs"`
	RuleSetUID             string    `json:"rule_set_uid"`
	RuleSetContentHash     string    `json:"rule_set_content_hash"`
	BinaryURL              string    `json:"binary_url"`
	WasmSHA256             string    `json:"wasm_sha256"`
	VM                     vmVersion `json:"vm"`
}

func engineVersion(in EngineInput, driverType, emptyRulesPolicy, failurePolicy, runtime, vmID string, codeTimeout time.Duration) (string, error) {
	raw, err := CanonicalJSON(engineVersionInput{
		SchemaVersion:          SchemaVersion,
		RenderingSchemaVersion: RenderingSchemaVersion,
		Engine:                 in.Name,
		Namespace:              in.Namespace,
		EngineUID:              string(in.UID),
		DriverType:             driverType,
		EmptyRulesPolicy:       emptyRulesPolicy,
		FailurePolicy:          failurePolicy,
		EnableFilterStateLogs:  in.EnableFilterStateLogs,
		RuleSetUID:             string(in.RuleSet.UID),
		RuleSetContentHash:     in.RuleSet.ContentHash,
		BinaryURL:              in.Binary.URL,
		WasmSHA256:             in.Binary.SHA256,
		VM: vmVersion{
			Runtime:     runtime,
			VMID:        vmID,
			CodeTimeout: codeTimeout.String(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("hash Engine configuration: %w", err)
	}
	return digest(raw), nil
}

func buildResource(name string, binary BinaryRef, runtime, vmID string, codeTimeout time.Duration, failurePolicy wasmv3.FailurePolicy, payload []byte) (*corev3.TypedExtensionConfig, error) {
	configuration, err := anypb.New(wrapperspb.String(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("wrap runtime configuration: %w", err)
	}

	pluginConfig := &wasmv3.PluginConfig{
		Name:          name,
		RootId:        name,
		FailurePolicy: failurePolicy,
		Configuration: configuration,
		Vm: &wasmv3.PluginConfig_VmConfig{
			VmConfig: &wasmv3.VmConfig{
				VmId:    vmID,
				Runtime: runtime,
				// ECDS delivers this config dynamically; NACK the update until
				// the remote code is cached instead of applying a filter that
				// cannot load and fails closed.
				NackOnCodeCacheMiss: true,
				Code: &corev3.AsyncDataSource{
					Specifier: &corev3.AsyncDataSource_Remote{
						Remote: &corev3.RemoteDataSource{
							HttpUri: &corev3.HttpUri{
								Uri: binary.URL,
								HttpUpstreamType: &corev3.HttpUri_Cluster{
									Cluster: wasmCluster,
								},
								Timeout: durationpb.New(codeTimeout),
							},
							Sha256: binary.SHA256,
						},
					},
				},
			},
		},
	}

	typedConfig, err := anypb.New(&wasmhttp.Wasm{Config: pluginConfig})
	if err != nil {
		return nil, fmt.Errorf("wrap WASM config: %w", err)
	}

	return &corev3.TypedExtensionConfig{
		Name:        name,
		TypedConfig: typedConfig,
	}, nil
}

func validateBinaryRef(binary BinaryRef) error {
	if binary.URL == "" {
		return errors.New("WASM binary URL is required")
	}
	parsed, err := url.Parse(binary.URL)
	if err != nil {
		return fmt.Errorf("parse WASM binary URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("WASM binary URL %q must be an https URL without userinfo, query, or fragment", binary.URL)
	}
	if !sha256HexPattern.MatchString(binary.SHA256) {
		return fmt.Errorf("WASM sha256 %q must be 64 lowercase hex characters", binary.SHA256)
	}
	return nil
}

func resolveDriverType(driverType string) (string, error) {
	if driverType == "" {
		return defaultDriverType, nil
	}
	if driverType != defaultDriverType {
		return "", fmt.Errorf("unsupported driver type %q", driverType)
	}
	return driverType, nil
}

func resolveFailurePolicy(failurePolicy string) (string, wasmv3.FailurePolicy, error) {
	switch failurePolicy {
	case "":
		return defaultFailurePolicy, wasmv3.FailurePolicy_FAIL_CLOSED, nil
	case "fail":
		return failurePolicy, wasmv3.FailurePolicy_FAIL_CLOSED, nil
	case "allow":
		return failurePolicy, wasmv3.FailurePolicy_FAIL_OPEN, nil
	default:
		return "", 0, fmt.Errorf("unsupported failure policy %q", failurePolicy)
	}
}

func resolveEmptyRulesPolicy(policy string) (string, error) {
	switch policy {
	case "":
		return string(assemble.EmptyRulesPolicyReject), nil
	case string(assemble.EmptyRulesPolicyReject), string(assemble.EmptyRulesPolicyAllow):
		return policy, nil
	default:
		return "", fmt.Errorf("unsupported empty rules policy %q", policy)
	}
}

func resolveVMSettings(settings VMSettings, engineUID types.UID) (runtime, vmID string, codeTimeout time.Duration) {
	runtime = settings.Runtime
	if runtime == "" {
		runtime = defaultRuntime
	}

	vmID = settings.VMID
	if vmID == "" {
		vmID = "coraza-" + string(engineUID)
	}

	codeTimeout = settings.CodeTimeout
	if codeTimeout <= 0 {
		codeTimeout = defaultCodeTimeout
	}
	return runtime, vmID, codeTimeout
}

func resourceName(namespace, engineName string, engineUID types.UID) string {
	return fmt.Sprintf("coraza.waf.%s.%s.%s", namespace, engineName, engineUID)
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
