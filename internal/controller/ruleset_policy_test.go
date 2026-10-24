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

func newRuleSetWithPolicy(namespace, name string, policy wafv1alpha1.EmptyRulesPolicy) *wafv1alpha1.RuleSet {
	return &wafv1alpha1.RuleSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: wafv1alpha1.RuleSetSpec{
			Sources:          []wafv1alpha1.SourceReference{{Name: "source"}},
			EmptyRulesPolicy: policy,
		},
	}
}

func TestRuleSetEmptyRulesPolicyDefault(t *testing.T) {
	ctx := context.Background()
	name := "policy-default"
	t.Cleanup(func() {
		rs := &wafv1alpha1.RuleSet{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}}
		if err := k8sClient.Delete(ctx, rs); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	})

	if err := k8sClient.Create(ctx, newRuleSetWithPolicy(testNamespace, name, "")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var ruleSet wafv1alpha1.RuleSet
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, &ruleSet); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if ruleSet.Spec.EmptyRulesPolicy != wafv1alpha1.EmptyRulesPolicyReject {
		t.Fatalf("emptyRulesPolicy = %q, want %q", ruleSet.Spec.EmptyRulesPolicy, wafv1alpha1.EmptyRulesPolicyReject)
	}
}

func TestRuleSetEmptyRulesPolicyAllow(t *testing.T) {
	ctx := context.Background()
	name := "policy-allow"
	t.Cleanup(func() {
		rs := &wafv1alpha1.RuleSet{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}}
		if err := k8sClient.Delete(ctx, rs); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	})

	if err := k8sClient.Create(ctx, newRuleSetWithPolicy(testNamespace, name, wafv1alpha1.EmptyRulesPolicyAllow)); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var ruleSet wafv1alpha1.RuleSet
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, &ruleSet); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if ruleSet.Spec.EmptyRulesPolicy != wafv1alpha1.EmptyRulesPolicyAllow {
		t.Fatalf("emptyRulesPolicy = %q, want %q", ruleSet.Spec.EmptyRulesPolicy, wafv1alpha1.EmptyRulesPolicyAllow)
	}
}

func TestRuleSetEmptyRulesPolicyRejectsUnknownValue(t *testing.T) {
	ctx := context.Background()
	name := "policy-invalid"
	t.Cleanup(func() {
		rs := &wafv1alpha1.RuleSet{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}}
		if err := k8sClient.Delete(ctx, rs); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	})

	err := k8sClient.Create(ctx, newRuleSetWithPolicy(testNamespace, name, "Deny"))
	if err == nil {
		t.Fatal("Create() error = nil, want rejection")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("Create() error = %v, want Invalid", err)
	}
	if !strings.Contains(err.Error(), "Unsupported value") {
		t.Fatalf("Create() error = %q, want enum rejection", err)
	}
}
