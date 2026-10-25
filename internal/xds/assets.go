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
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

const (
	assetPathPrefix   = "/wasm/"
	assetPathSuffix   = ".wasm"
	wasmContentType   = "application/wasm"
	immutableCacheMax = "public, max-age=31536000, immutable"
)

var assetNamePattern = regexp.MustCompile(`^[0-9a-f]{64}\.wasm$`)

// AssetHandler serves the single bundled WASM module by digest. Only manifest
// entries are addressable; the handler never touches the filesystem, never
// redirects, and requires no bearer token.
type AssetHandler struct {
	manifest *assets.Manifest
	data     []byte
	metrics  *Metrics
}

// NewAssetHandler returns a handler for the verified bundled module.
func NewAssetHandler(manifest *assets.Manifest, data []byte, metrics *Metrics) *AssetHandler {
	return &AssetHandler{manifest: manifest, data: data, metrics: metrics}
}

// ServeHTTP implements http.Handler.
func (h *AssetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	code := http.StatusNotFound
	defer func() { h.metrics.AssetRequest(code) }()

	if r.Method != http.MethodGet {
		code = http.StatusMethodNotAllowed
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(code)
		return
	}

	name, ok := assetNameFromPath(r.URL.Path)
	if !ok || h.manifest == nil || name != h.manifest.SHA256+assetPathSuffix {
		w.WriteHeader(code)
		return
	}

	code = http.StatusOK
	w.Header().Set("Content-Type", wasmContentType)
	w.Header().Set("Cache-Control", immutableCacheMax)
	w.Header().Set("Content-Length", strconv.Itoa(len(h.data)))
	w.WriteHeader(code)
	if _, err := w.Write(h.data); err != nil {
		return
	}
}

func assetNameFromPath(path string) (string, bool) {
	if !strings.HasPrefix(path, assetPathPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(path, assetPathPrefix)
	if !assetNamePattern.MatchString(name) {
		return "", false
	}
	return name, true
}
