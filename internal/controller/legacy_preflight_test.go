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

package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

func TestLegacyWasmFields(t *testing.T) {
	tests := []struct {
		name       string
		wasm       map[string]any
		wantImage  bool
		wantSecret bool
	}{
		{
			name: "no driver",
		},
		{
			name: "new fields only",
			wasm: map[string]any{"url": "https://example.com/plugin.wasm", "sha256": testSHA256},
		},
		{
			name:      "image only",
			wasm:      map[string]any{"image": "oci://example.com/plugin:1"},
			wantImage: true,
		},
		{
			name:       "imagePullSecret only",
			wasm:       map[string]any{"imagePullSecret": "registry-creds"},
			wantSecret: true,
		},
		{
			name:       "both legacy fields",
			wasm:       map[string]any{"image": "oci://example.com/plugin:1", "imagePullSecret": "registry-creds"},
			wantImage:  true,
			wantSecret: true,
		},
		{
			name:      "empty image is still a legacy field",
			wasm:      map[string]any{"image": ""},
			wantImage: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := map[string]any{}
			if tt.wasm != nil {
				object["spec"] = map[string]any{"driver": map[string]any{"wasm": tt.wasm}}
			}
			engine := &unstructured.Unstructured{Object: object}

			gotImage, gotSecret := legacyWasmFields(engine)
			if gotImage != tt.wantImage || gotSecret != tt.wantSecret {
				t.Fatalf("legacyWasmFields() = (%v, %v), want (%v, %v)", gotImage, gotSecret, tt.wantImage, tt.wantSecret)
			}
		})
	}
}

func TestLegacyEnginePreflight(t *testing.T) {
	ctx := context.Background()
	name := "legacy-engine"
	t.Cleanup(func() {
		engine := &wafv1alpha1.Engine{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}}
		if err := k8sClient.Delete(ctx, engine); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete Engine: %v", err)
		}
	})

	engine := newEngineWithWasm(testNamespace, name, &wafv1alpha1.WasmDriverConfig{
		Image: "oci://example.com/plugin:1",
	})
	if err := k8sClient.Create(ctx, engine); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Use an uncached, direct client like the manager's APIReader.
	apiReader, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Failed to create API reader: %v", err)
	}

	metrics, err := NewCorazaMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewCorazaMetrics() error = %v", err)
	}
	recorder := events.NewFakeRecorder(10)

	// FakeRecorder.Eventf blocks once its channel is full. The preflight lists
	// Engines cluster-wide, so other tests' Engines can fill it; drain
	// concurrently instead of assuming only this test's Engine exists.
	var (
		eventsMu sync.Mutex
		recorded []string
	)
	go func() {
		for event := range recorder.Events {
			eventsMu.Lock()
			recorded = append(recorded, event)
			eventsMu.Unlock()
		}
	}()

	preflight := &LegacyEnginePreflight{
		Reader:   apiReader,
		Recorder: recorder,
		Metrics:  metrics,
	}
	if err := preflight.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if got := testutil.ToFloat64(metrics.legacyWasmEngines); got < 1 {
		t.Fatalf("coraza_legacy_wasm_engines = %v, want at least 1", got)
	}

	var found bool
	eventsMu.Lock()
	for _, event := range recorded {
		if strings.Contains(event, ReasonLegacyWasmImageFields) {
			found = true
			break
		}
	}
	foundEvents := append([]string(nil), recorded...)
	eventsMu.Unlock()
	if !found {
		t.Fatalf("events = %v, want reason %q", foundEvents, ReasonLegacyWasmImageFields)
	}

	preflight.FailOnLegacy = true
	err = preflight.Start(ctx)
	if err == nil {
		t.Fatal("Start() error = nil, want legacy Engine failure")
	}
	if !strings.Contains(err.Error(), "use removed spec.driver.wasm.image/imagePullSecret") {
		t.Fatalf("Start() error = %q, want legacy field message", err)
	}
}

