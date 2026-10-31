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

// Package assets resolves the WASM modules bundled with the operator.
//
// The committed module is a development fixture; release builds replace
// plugin.wasm and plugin.manifest.yaml with the published module and manifest.
package assets

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// -----------------------------------------------------------------------------
// Constants
// -----------------------------------------------------------------------------

// ManifestFile is the file name of the bundle manifest inside the assets
// filesystem.
const ManifestFile = "plugin.manifest.yaml"

// SupportedSchemaVersion is the manifest schema version understood by this
// operator.
const SupportedSchemaVersion = 1

// -----------------------------------------------------------------------------
// Embedded assets
// -----------------------------------------------------------------------------

//go:embed plugin.wasm plugin.manifest.yaml
var embedded embed.FS

// -----------------------------------------------------------------------------
// Manifest
// -----------------------------------------------------------------------------

// Manifest describes the WASM module bundled with the operator.
type Manifest struct {
	// SchemaVersion is the manifest schema version.
	SchemaVersion int `yaml:"schema_version"`

	// Module is the module name.
	Module string `yaml:"module"`

	// File is the path of the raw WASM module relative to the manifest.
	File string `yaml:"file"`

	// SHA256 is the lowercase hexadecimal SHA-256 of File.
	SHA256 string `yaml:"sha256"`

	// GitSHA is the source revision the module was built from.
	GitSHA string `yaml:"git_sha"`

	// CorazaVersion is the Coraza version linked into the module.
	CorazaVersion string `yaml:"coraza_version"`

	// ProxyWasmSDKVersion is the proxy-wasm SDK version linked into the module.
	ProxyWasmSDKVersion string `yaml:"proxy_wasm_sdk_version"`

	// CRSVersion is the OWASP CoreRuleSet release embedded in the module, or
	// empty when the module was built without the embed-crs feature.
	CRSVersion string `yaml:"crs_version,omitempty"`
}

// Load reads, decodes, and shape-validates the manifest from fsys.
func Load(fsys fs.FS) (*Manifest, error) {
	data, err := fs.ReadFile(fsys, ManifestFile)
	if err != nil {
		return nil, fmt.Errorf("read asset manifest: %w", err)
	}

	var manifest Manifest
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode asset manifest %s: %w", ManifestFile, err)
	}
	if err := manifest.validate(); err != nil {
		return nil, fmt.Errorf("invalid asset manifest %s: %w", ManifestFile, err)
	}

	return &manifest, nil
}

func (m *Manifest) validate() error {
	if m.SchemaVersion != SupportedSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d, want %d", m.SchemaVersion, SupportedSchemaVersion)
	}
	if m.File == "" {
		return errors.New("file must be set")
	}
	if len(m.SHA256) != sha256.Size*2 {
		return fmt.Errorf("sha256 must be %d lowercase hexadecimal characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(m.SHA256); err != nil {
		return fmt.Errorf("sha256 must be hexadecimal: %w", err)
	}
	if strings.ToLower(m.SHA256) != m.SHA256 {
		return errors.New("sha256 must be lowercase")
	}
	return nil
}

// -----------------------------------------------------------------------------
// Resolution
// -----------------------------------------------------------------------------

// VerifyAll verifies the digest of every module file declared by the manifest.
func VerifyAll(fsys fs.FS, manifest *Manifest) error {
	if manifest == nil {
		return errors.New("nil asset manifest")
	}
	data, err := fs.ReadFile(fsys, manifest.File)
	if err != nil {
		return fmt.Errorf("read module %q: %w", manifest.File, err)
	}
	sum := sha256.Sum256(data)
	if digest := hex.EncodeToString(sum[:]); digest != manifest.SHA256 {
		return fmt.Errorf("module %q digest mismatch: manifest %s, file %s", manifest.File, manifest.SHA256, digest)
	}
	return nil
}

// ResolveDefault returns the embedded manifest and the verified bytes of the
// default bundled module.
//
// The embedded bundle is immutable, so the manifest metadata and digest are
// computed once. Callers must not mutate the returned manifest or bytes.
func ResolveDefault() (*Manifest, []byte, error) {
	bundle, err := resolveDefault()
	if err != nil {
		return nil, nil, err
	}
	return bundle.manifest, bundle.data, nil
}

type defaultBundle struct {
	manifest *Manifest
	data     []byte
}

var resolveDefault = sync.OnceValues(func() (defaultBundle, error) {
	manifest, err := Load(embedded)
	if err != nil {
		return defaultBundle{}, err
	}
	if err := VerifyAll(embedded, manifest); err != nil {
		return defaultBundle{}, err
	}
	data, err := fs.ReadFile(embedded, manifest.File)
	if err != nil {
		return defaultBundle{}, fmt.Errorf("read bundled module %q: %w", manifest.File, err)
	}
	return defaultBundle{manifest: manifest, data: data}, nil
})
