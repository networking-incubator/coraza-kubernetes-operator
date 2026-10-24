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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

const testSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newEngineWithWasm(namespace, name string, wasm *wafv1alpha1.WasmDriverConfig) *wafv1alpha1.Engine {
	return &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: wafv1alpha1.EngineSpec{
			RuleSet: wafv1alpha1.RuleSetReference{Name: "ruleset"},
			Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "gateway",
			},
			Driver: wafv1alpha1.DriverConfig{
				Type: wafv1alpha1.DriverTypeWasm,
				Wasm: wasm,
			},
		},
	}
}

func deleteEngine(ctx context.Context, t *testing.T, namespace, name string) {
	t.Helper()
	engine := &wafv1alpha1.Engine{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if err := k8sClient.Delete(ctx, engine); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("Failed to delete Engine %s/%s: %v", namespace, name, err)
	}
}

func TestEngineWasmDriverCEL(t *testing.T) {
	tests := []struct {
		name    string
		wasm    *wafv1alpha1.WasmDriverConfig
		wantErr string
	}{
		{
			name: "https url with sha256",
			wasm: &wafv1alpha1.WasmDriverConfig{
				URL:    "https://example.com/plugin.wasm",
				SHA256: testSHA256,
			},
		},
		{
			name: "both omitted uses bundled module",
			wasm: &wafv1alpha1.WasmDriverConfig{},
		},
		{
			name: "legacy image remains valid in phase 2",
			wasm: &wafv1alpha1.WasmDriverConfig{
				Image: "oci://example.com/plugin:1",
			},
		},
		{
			name:    "url without sha256",
			wasm:    &wafv1alpha1.WasmDriverConfig{URL: "https://example.com/plugin.wasm"},
			wantErr: "url and sha256 must be set together",
		},
		{
			name:    "sha256 without url",
			wasm:    &wafv1alpha1.WasmDriverConfig{SHA256: testSHA256},
			wantErr: "url and sha256 must be set together",
		},
		{
			name: "http url",
			wasm: &wafv1alpha1.WasmDriverConfig{
				URL:    "http://example.com/plugin.wasm",
				SHA256: testSHA256,
			},
			wantErr: "url must be an https URL",
		},
		{
			name: "url with query",
			wasm: &wafv1alpha1.WasmDriverConfig{
				URL:    "https://example.com/plugin.wasm?version=1",
				SHA256: testSHA256,
			},
			wantErr: "url must be an https URL without query or fragment",
		},
		{
			name: "url with fragment",
			wasm: &wafv1alpha1.WasmDriverConfig{
				URL:    "https://example.com/plugin.wasm#fragment",
				SHA256: testSHA256,
			},
			wantErr: "url must be an https URL without query or fragment",
		},
		{
			name: "uppercase sha256",
			wasm: &wafv1alpha1.WasmDriverConfig{
				URL:    "https://example.com/plugin.wasm",
				SHA256: strings.ToUpper(testSHA256),
			},
			wantErr: "sha256 must be 64 lowercase hexadecimal characters",
		},
		{
			name: "short sha256",
			wasm: &wafv1alpha1.WasmDriverConfig{
				URL:    "https://example.com/plugin.wasm",
				SHA256: testSHA256[:63],
			},
			wantErr: "sha256 must be 64 lowercase hexadecimal characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			name := "cel-engine"
			t.Cleanup(func() { deleteEngine(ctx, t, testNamespace, name) })

			err := k8sClient.Create(ctx, newEngineWithWasm(testNamespace, name, tt.wasm))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Create() error = %v, want success", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Create() error = nil, want rejection")
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("Create() error = %v, want Invalid", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Create() error = %q, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestEngineWasmDriverCELUpdate(t *testing.T) {
	ctx := context.Background()
	name := "cel-engine-update"
	t.Cleanup(func() { deleteEngine(ctx, t, testNamespace, name) })

	engine := newEngineWithWasm(testNamespace, name, &wafv1alpha1.WasmDriverConfig{
		URL:    "https://example.com/plugin.wasm",
		SHA256: testSHA256,
	})
	if err := k8sClient.Create(ctx, engine); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var current wafv1alpha1.Engine
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, &current); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	current.Spec.Driver.Wasm.SHA256 = "not-a-digest"

	err := k8sClient.Update(ctx, &current)
	if err == nil {
		t.Fatal("Update() error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "sha256 must be 64 lowercase hexadecimal characters") {
		t.Fatalf("Update() error = %q, want sha256 rejection", err)
	}
}
