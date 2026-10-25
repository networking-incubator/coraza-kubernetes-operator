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
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

func newAssetHandler(t *testing.T) (*AssetHandler, *assets.Manifest, []byte) {
	t.Helper()
	manifest, data, err := assets.ResolveDefault()
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return NewAssetHandler(manifest, data, metrics), manifest, data
}

func TestAssetHandlerServesManifestEntry(t *testing.T) {
	handler, manifest, data := newAssetHandler(t)

	request := httptest.NewRequest(http.MethodGet, assetPathPrefix+manifest.SHA256+assetPathSuffix, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != wasmContentType {
		t.Fatalf("Content-Type = %q, want %q", got, wasmContentType)
	}
	if got := recorder.Header().Get("Cache-Control"); got != immutableCacheMax {
		t.Fatalf("Cache-Control = %q, want %q", got, immutableCacheMax)
	}
	if recorder.Body.String() != string(data) {
		t.Fatal("body does not match the bundled module")
	}
}

func TestAssetHandlerRejectsUnknownAndMalformedPaths(t *testing.T) {
	handler, manifest, _ := newAssetHandler(t)

	validDigest := manifest.SHA256
	cases := map[string]string{
		"unknown digest": assetPathPrefix + "0000000000000000000000000000000000000000000000000000000000000000" + assetPathSuffix,
		"uppercase":      assetPathPrefix + "ABCDEF0000000000000000000000000000000000000000000000000000000000" + assetPathSuffix,
		"short digest":   assetPathPrefix + "abcd" + assetPathSuffix,
		"no suffix":      assetPathPrefix + validDigest,
		"plain name":     assetPathPrefix + "plugin.wasm",
		"legacy rules":   "/rules/shop/checkout",
		"directory":      assetPathPrefix,
		"traversal":      assetPathPrefix + "..%2f" + validDigest + assetPathSuffix,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", recorder.Code)
			}
			if location := recorder.Header().Get("Location"); location != "" {
				t.Fatalf("Location = %q, want no redirect", location)
			}
		})
	}
}

func TestAssetHandlerRejectsNonGet(t *testing.T) {
	handler, manifest, _ := newAssetHandler(t)
	request := httptest.NewRequest(http.MethodPost, assetPathPrefix+manifest.SHA256+assetPathSuffix, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

func TestAssetHandlerRequiresNoAuthorization(t *testing.T) {
	handler, manifest, _ := newAssetHandler(t)
	request := httptest.NewRequest(http.MethodGet, assetPathPrefix+manifest.SHA256+assetPathSuffix, nil)
	if request.Header.Get("Authorization") != "" {
		t.Fatal("test request unexpectedly carries credentials")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}
