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

package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func manifestFS(manifest, module []byte) fstest.MapFS {
	return fstest.MapFS{
		ManifestFile:  {Data: manifest},
		"plugin.wasm": {Data: module},
	}
}

func manifestFor(sum string, schemaVersion int) string {
	return fmt.Sprintf(`schema_version: %d
module: test
file: plugin.wasm
sha256: %s
git_sha: dev
coraza_version: dev
proxy_wasm_sdk_version: dev
`, schemaVersion, sum)
}

func TestResolveDefault(t *testing.T) {
	manifest, data, err := ResolveDefault()
	if err != nil {
		t.Fatalf("ResolveDefault() error = %v", err)
	}
	if manifest.SchemaVersion != SupportedSchemaVersion {
		t.Errorf("schema version = %d, want %d", manifest.SchemaVersion, SupportedSchemaVersion)
	}
	if got := len(data); got != 8 {
		t.Errorf("module length = %d, want 8", got)
	}
	if !bytes.HasPrefix(data, []byte("\x00asm\x01\x00\x00\x00")) {
		t.Errorf("module does not start with a valid empty WASM header: %x", data)
	}
	if got := digest(data); got != manifest.SHA256 {
		t.Errorf("manifest sha256 = %s, want %s", manifest.SHA256, got)
	}
}

func TestLoad(t *testing.T) {
	module := []byte("\x00asm\x01\x00\x00\x00")
	tests := []struct {
		name     string
		manifest string
		wantErr  string
	}{
		{
			name:     "valid",
			manifest: manifestFor(digest(module), SupportedSchemaVersion),
		},
		{
			name:     "unsupported schema version",
			manifest: manifestFor(digest(module), 2),
			wantErr:  "unsupported schema_version",
		},
		{
			name:     "uppercase sha256",
			manifest: manifestFor(strings.ToUpper(digest(module)), SupportedSchemaVersion),
			wantErr:  "sha256 must be lowercase",
		},
		{
			name:     "non-hex sha256",
			manifest: manifestFor("zzz", SupportedSchemaVersion),
			wantErr:  "sha256 must be 64 lowercase hexadecimal characters",
		},
		{
			name:     "unknown field",
			manifest: manifestFor(digest(module), SupportedSchemaVersion) + "extra: true\n",
			wantErr:  "field extra not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(manifestFS([]byte(tt.manifest), module))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Load() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() error = nil, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %q, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyAll(t *testing.T) {
	module := []byte("\x00asm\x01\x00\x00\x00")
	manifest := &Manifest{File: "plugin.wasm", SHA256: digest(module)}
	if err := VerifyAll(manifestFS(nil, module), manifest); err != nil {
		t.Fatalf("VerifyAll() error = %v", err)
	}

	manifest.SHA256 = digest([]byte("other"))
	err := VerifyAll(manifestFS(nil, module), manifest)
	if err == nil {
		t.Fatal("VerifyAll() error = nil, want digest mismatch")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("VerifyAll() error = %q, want digest mismatch", err)
	}

	if err := VerifyAll(manifestFS(nil, module), nil); err == nil {
		t.Fatal("VerifyAll(nil) error = nil, want error")
	}
}

// TestEmbeddedBundle reproduces the `make wasm.bundle`/`make wasm.verify`
// contract in Go: the committed bytes must hash to the committed manifest, and a
// manifest whose digest no longer matches the bundled file must fail.
func TestEmbeddedBundle(t *testing.T) {
	manifest, err := Load(embedded)
	if err != nil {
		t.Fatalf("Load(embedded) error = %v", err)
	}
	if manifest.File != "plugin.wasm" {
		t.Errorf("manifest file = %q, want plugin.wasm (the file wasm.bundle writes)", manifest.File)
	}
	if manifest.Module == "" {
		t.Error("manifest module is empty")
	}

	module, err := fs.ReadFile(embedded, manifest.File)
	if err != nil {
		t.Fatalf("read embedded module: %v", err)
	}
	if got := digest(module); got != manifest.SHA256 {
		t.Errorf("bundled module digest = %s, manifest sha256 = %s; run make wasm.bundle", got, manifest.SHA256)
	}

	stale := *manifest
	stale.SHA256 = digest(append(module, 0))
	if err := VerifyAll(embedded, &stale); err == nil {
		t.Fatal("VerifyAll(stale manifest) error = nil, want digest mismatch")
	}
}
