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
	"encoding/json"
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
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
)

const (
	// SchemaVersion is the runtime configuration schema version understood by
	// the envoy-coraza plugin. Version 2 uses the coraza-rs configuration keys
	// (directives_map, default_directives, rule_data) instead of the legacy
	// flat rules/data_files shape.
	SchemaVersion = 2
	// RenderingSchemaVersion is the rendering schema version. It is part of
	// every engine version hash. Version 4 switches the payload to the
	// coraza-rs configuration schema.
	RenderingSchemaVersion = 4
	// MaxConfigurationBytes is the envoy-coraza plugin's hard cap on one
	// plugin configuration document (8 MiB). A larger ECDS payload is NACKed,
	// so publication rejects it before it reaches a proxy.
	MaxConfigurationBytes = 8 << 20
	// defaultPolicyName is the directives_map policy every Engine uses. The
	// operator renders one policy and selects it as the default.
	defaultPolicyName = "default"

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

// ErrAcceptedMismatch is wrapped by Render and Matches when a rendered
// artifact does not match the Engine's accepted configuration.
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
	Payload      []byte // deterministic runtime configuration JSON
	Resource     *corev3.TypedExtensionConfig

	// EmptyRulesPolicy, DriverType, EnableFilterStateLogs, and VM are the
	// resolved rendering inputs of this artifact, with all defaults applied.
	// The Engine's accepted configuration records them so a replica can
	// reconstruct the exact artifact without reading the live spec; they are
	// never recomputed from it.
	EmptyRulesPolicy      string // "Reject" | "Allow"
	DriverType            string // "wasm"
	EnableFilterStateLogs bool
	VM                    VMSettings

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

	raw, err := json.Marshal(struct {
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
	// envoy-coraza has no embedded alias filesystem or glob expansion, so an
	// Include the previous Go plugin resolved would NACK at configure time
	// while the Engine reported Ready.
	if err := validation.ValidateCorazaRSIncludes(in.Content.Rules, in.Content.DataFiles); err != nil {
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
		// A content divergence from the accepted record is the canonical
		// hash mismatch: publication counts it via ErrAcceptedMismatch.
		return Artifact{}, fmt.Errorf("%w: assembled content hash %s does not match accepted RuleSet content hash %s",
			ErrAcceptedMismatch, contentHash, in.RuleSet.ContentHash)
	}
	if emptyRulesPolicy == string(assemble.EmptyRulesPolicyReject) && strings.TrimSpace(in.Content.Rules) == "" {
		return Artifact{}, fmt.Errorf("%w: Engine %s/%s has empty rules and empty rules policy is Reject", assemble.ErrEmptyRules, in.Namespace, in.Name)
	}

	runtime, vmID, codeTimeout := resolveVMSettings(in.VMSettings, in.UID)
	resourceName := ResourceName(in.Namespace, in.Name, in.UID)

	// The payload carries the RuleData files so the plugin can rebuild a WAF
	// whose @pmFromFile references resolve.
	dataFiles := make(map[string][]byte, len(in.Content.DataFiles))
	for name, data := range in.Content.DataFiles {
		if data == nil {
			data = []byte{}
		}
		dataFiles[name] = data
	}
	// envoy-coraza reads rule_data as a map of file name to file bytes
	// encoded as JSON strings. RuleData is string-valued in the API, so this
	// round-trips its UTF-8 bytes without loss.
	ruleData := make(map[string]string, len(dataFiles))
	for name, data := range dataFiles {
		ruleData[name] = string(data)
	}
	// json.Marshal is deterministic for these types — struct fields keep
	// their declaration order and encoding/json sorts map keys — so every
	// replica renders identical payload bytes.
	payload, err := json.Marshal(runtimeConfig{
		SchemaVersion:         SchemaVersion,
		Engine:                in.Name,
		Namespace:             in.Namespace,
		EngineUID:             string(in.UID),
		DriverType:            driverType,
		EmptyRulesPolicy:      emptyRulesPolicy,
		FailurePolicy:         failurePolicy,
		EnableFilterStateLogs: in.EnableFilterStateLogs,
		DirectivesMap:         map[string][]string{defaultPolicyName: {in.Content.Rules}},
		DefaultDirectives:     defaultPolicyName,
		RuleData:              ruleData,
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
		ResourceName:          resourceName,
		Version:               version,
		Payload:               payload,
		Resource:              resource,
		EmptyRulesPolicy:      emptyRulesPolicy,
		DriverType:            driverType,
		EnableFilterStateLogs: in.EnableFilterStateLogs,
		VM:                    VMSettings{Runtime: runtime, VMID: vmID, CodeTimeout: codeTimeout},
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
// configuration. A replica must not publish on a mismatch. Version is a hash
// over every other compared field, so the remaining rows exist purely for
// diagnostics.
func Matches(accepted AcceptedEngine, a Artifact) error {
	rows := []struct {
		format string
		want   any
		got    any
	}{
		{"rendering schema version: accepted %d, rendered %d", accepted.RenderingSchemaVersion, a.identity.renderingSchemaVersion},
		{"version: accepted %q, rendered %q", accepted.Version, a.Version},
		{"RuleSet uid: accepted %q, rendered %q", accepted.RuleSetUID, a.identity.ruleSetUID},
		{"RuleSet content hash: accepted %q, rendered %q", accepted.RuleSetContentHash, a.identity.ruleSetContentHash},
		{"binary URL: accepted %q, rendered %q", accepted.BinaryURL, a.identity.binaryURL},
		{"WASM sha256: accepted %q, rendered %q", accepted.WasmSHA256, a.identity.wasmSHA256},
		{"failure policy: accepted %q, rendered %q", accepted.FailurePolicy, a.identity.failurePolicy},
	}

	var mismatches []error
	for _, r := range rows {
		if r.want != r.got {
			mismatches = append(mismatches, fmt.Errorf(r.format, r.want, r.got))
		}
	}

	if len(mismatches) > 0 {
		return fmt.Errorf("%w: %w", ErrAcceptedMismatch, errors.Join(mismatches...))
	}
	return nil
}

// runtimeConfig is the schema_version 2 JSON passed to envoy-coraza. The
// operator's own identity fields are informational; the plugin reads
// directives_map, default_directives, rule_data and enable_filter_state_logs
// and ignores the rest.
type runtimeConfig struct {
	SchemaVersion         int                 `json:"schema_version"`
	Engine                string              `json:"engine"`
	Namespace             string              `json:"namespace"`
	EngineUID             string              `json:"engine_uid"`
	DriverType            string              `json:"driver_type"`
	EmptyRulesPolicy      string              `json:"empty_rules_policy"`
	FailurePolicy         string              `json:"failure_policy"`
	EnableFilterStateLogs bool                `json:"enable_filter_state_logs"`
	DirectivesMap         map[string][]string `json:"directives_map"`
	DefaultDirectives     string              `json:"default_directives"`
	RuleData              map[string]string   `json:"rule_data"`
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
	// Deterministic for the same reasons as the runtime payload. This is
	// not RFC 8785 canonical JSON, which is fine while Go is the only
	// hasher of this configuration; the plugin parses the payload but never
	// re-hashes it.
	raw, err := json.Marshal(engineVersionInput{
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
		// envoy-coraza pauses request headers to inspect the body. Envoy
		// rejects that StopIteration unless the plugin is allowed to use it,
		// and a request with a body then hangs until the client times out.
		AllowOnHeadersStopIteration: wrapperspb.Bool(true),
		Vm: &wasmv3.PluginConfig_VmConfig{
			VmConfig: &wasmv3.VmConfig{
				VmId:    vmID,
				Runtime: runtime,
				// A proxy without the module in its cache must reject the push
				// instead of installing a filter that cannot load; the operator
				// then resubscribes it once the fetch completes.
				NackOnCodeCacheMiss: true,
				Code:                wasmCode(binary, codeTimeout),
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

// wasmCode returns the sha256-pinned remote source. A proxy fetches the module
// once and caches it by digest; the operator resubscribes it when it rejects a
// push because the bytes are not cached yet.
func wasmCode(binary BinaryRef, codeTimeout time.Duration) *corev3.AsyncDataSource {
	return &corev3.AsyncDataSource{
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
	}
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

// ResourceName returns the ECDS resource name for an Engine. The name is
// load-bearing on both sides of the trust boundary: xds uses it as the
// authorization key for subscriptions and this package serves it as the
// resource name, so the format must stay single-sourced and never drift.
func ResourceName(namespace, engineName string, engineUID types.UID) string {
	return fmt.Sprintf("coraza.waf.%s.%s.%s", namespace, engineName, engineUID)
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
