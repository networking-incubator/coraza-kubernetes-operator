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

func TestEngineAcceptedConfigurationRoundTrip(t *testing.T) {
	ctx := context.Background()
	name := "status-engine"
	key := types.NamespacedName{Namespace: testNamespace, Name: name}
	t.Cleanup(func() {
		engine := &wafv1alpha1.Engine{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}}
		if err := k8sClient.Delete(ctx, engine); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete Engine: %v", err)
		}
	})

	if err := k8sClient.Create(ctx, newEngineWithWasm(testNamespace, name, &wafv1alpha1.WasmDriverConfig{
		URL:    "https://example.com/plugin.wasm",
		SHA256: testSHA256,
	})); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var engine wafv1alpha1.Engine
	if err := k8sClient.Get(ctx, key, &engine); err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	want := &wafv1alpha1.EngineAcceptedConfiguration{
		ObservedGeneration:     engine.Generation,
		RenderingSchemaVersion: 1,
		Version:                "sha256:" + strings.Repeat("a", 64),
		RuleSetUID:             "rule-set-uid",
		RuleSetContentHash:     "sha256:" + strings.Repeat("b", 64),
		BinaryURL:              "https://example.com/plugin.wasm",
		WasmSHA256:             testSHA256,
		FailurePolicy:          wafv1alpha1.FailurePolicyFail,
	}
	engine.Status = &wafv1alpha1.EngineStatus{AcceptedConfiguration: want}
	if err := k8sClient.Status().Update(ctx, &engine); err != nil {
		t.Fatalf("Status().Update() error = %v", err)
	}

	var got wafv1alpha1.Engine
	if err := k8sClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status == nil || got.Status.AcceptedConfiguration == nil {
		t.Fatal("status.acceptedConfiguration is nil after round-trip")
	}
	if *got.Status.AcceptedConfiguration != *want {
		t.Fatalf("acceptedConfiguration = %+v, want %+v", *got.Status.AcceptedConfiguration, *want)
	}
}

func TestRuleSetAcceptedContentRoundTrip(t *testing.T) {
	ctx := context.Background()
	name := "status-ruleset"
	key := types.NamespacedName{Namespace: testNamespace, Name: name}
	t.Cleanup(func() {
		ruleSet := &wafv1alpha1.RuleSet{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}}
		if err := k8sClient.Delete(ctx, ruleSet); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	})

	if err := k8sClient.Create(ctx, newRuleSetWithPolicy(testNamespace, name, "")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var ruleSet wafv1alpha1.RuleSet
	if err := k8sClient.Get(ctx, key, &ruleSet); err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	want := &wafv1alpha1.RuleSetAcceptedContent{
		ObservedGeneration: ruleSet.Generation,
		ContentHash:        "sha256:" + strings.Repeat("c", 64),
	}
	ruleSet.Status.AcceptedContent = want
	if err := k8sClient.Status().Update(ctx, &ruleSet); err != nil {
		t.Fatalf("Status().Update() error = %v", err)
	}

	var got wafv1alpha1.RuleSet
	if err := k8sClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status.AcceptedContent == nil {
		t.Fatal("status.acceptedContent is nil after round-trip")
	}
	if *got.Status.AcceptedContent != *want {
		t.Fatalf("acceptedContent = %+v, want %+v", *got.Status.AcceptedContent, *want)
	}
}
