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
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
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

// legacyListReader returns a synthetic Engine list for unstructured reads. It
// simulates objects stored under the old CRD, which the API server rejects once
// the tombstone validation is active.
type legacyListReader struct {
	client.Reader
	engines []unstructured.Unstructured
}

func (r *legacyListReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	u, ok := list.(*unstructured.UnstructuredList)
	if !ok {
		return fmt.Errorf("unexpected list type %T", list)
	}
	u.Items = append([]unstructured.Unstructured(nil), r.engines...)
	return nil
}

func TestLegacyEnginePreflight(t *testing.T) {
	ctx := context.Background()

	legacy := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": wafv1alpha1.GroupVersion.String(),
		"kind":       "Engine",
		"metadata": map[string]any{
			"namespace": testNamespace,
			"name":      "legacy-engine",
		},
		"spec": map[string]any{
			"driver": map[string]any{
				"wasm": map[string]any{"image": "oci://example.com/plugin:1"},
			},
		},
	}}
	reader := &legacyListReader{engines: []unstructured.Unstructured{legacy}}

	metrics, err := NewCorazaMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewCorazaMetrics() error = %v", err)
	}
	recorder := events.NewFakeRecorder(10)

	preflight := &LegacyEnginePreflight{
		Reader:   reader,
		Recorder: recorder,
		Metrics:  metrics,
	}
	if err := preflight.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if got := testutil.ToFloat64(metrics.legacyWasmEngines); got != 1 {
		t.Fatalf("coraza_legacy_wasm_engines = %v, want 1", got)
	}

	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, ReasonLegacyWasmImageFields) {
			t.Fatalf("event = %q, want reason %q", event, ReasonLegacyWasmImageFields)
		}
	default:
		t.Fatal("no event emitted for the legacy Engine")
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

// legacyEngineReader returns a synthetic raw Engine for unstructured reads and
// delegates everything else. It simulates an object stored under the old CRD,
// which the API server would reject once the tombstone validation is active.
type legacyEngineReader struct {
	client.Reader
	object *unstructured.Unstructured
}

func (r *legacyEngineReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		u.Object = r.object.DeepCopy().Object
		return nil
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// TestEngineReconciler_RejectsLegacyWasmFields verifies that a stored legacy
// Engine is marked Degraded before provisioning and never falls back to the
// bundled module.
func TestEngineReconciler_RejectsLegacyWasmFields(t *testing.T) {
	ctx := context.Background()
	name := "legacy-reconcile-engine"
	key := types.NamespacedName{Namespace: testNamespace, Name: name}
	t.Cleanup(func() {
		engine := &wafv1alpha1.Engine{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}}
		if err := k8sClient.Delete(ctx, engine); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete Engine: %v", err)
		}
	})

	engine := newEngineWithWasm(testNamespace, name, &wafv1alpha1.WasmDriverConfig{
		URL:    "https://assets.example.com/plugin.wasm",
		SHA256: testSHA256,
	})
	if err := k8sClient.Create(ctx, engine); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	raw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": wafv1alpha1.GroupVersion.String(),
		"kind":       "Engine",
		"metadata": map[string]any{
			"namespace": testNamespace,
			"name":      name,
		},
		"spec": map[string]any{
			"driver": map[string]any{
				"wasm": map[string]any{"image": "oci://example.com/plugin:1"},
			},
		},
	}}

	reconciler := &EngineReconciler{
		Client:    k8sClient,
		APIReader: &legacyEngineReader{Reader: k8sClient, object: raw},
		Scheme:    scheme,
		Recorder:  utils.NewTestRecorder(),
	}
	req := ctrl.Request{NamespacedName: key}

	// First reconcile adds the finalizer and requeues.
	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("first reconcile should requeue after adding the finalizer")
	}

	// Second reconcile applies the legacy field rejection.
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var updated wafv1alpha1.Engine
	if err := k8sClient.Get(ctx, key, &updated); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, conditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue || degraded.Reason != ReasonLegacyWasmImageFields {
		t.Fatalf("Degraded = %+v, want %s=True with reason %s", degraded, conditionDegraded, ReasonLegacyWasmImageFields)
	}
	if !strings.Contains(degraded.Message, "set url and sha256") {
		t.Fatalf("Degraded message = %q, want migration guidance", degraded.Message)
	}

	var envoyFilter unstructured.Unstructured
	envoyFilter.SetGroupVersionKind(envoyFilterGVK)
	err = k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: envoyFilterName(name)}, &envoyFilter)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("EnvoyFilter Get() error = %v, want NotFound", err)
	}
}
