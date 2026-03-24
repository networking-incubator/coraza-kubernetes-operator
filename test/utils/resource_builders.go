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

// Package utils provides typed resource builders for unit tests (internal/controller, internal/rulesets).
// Integration tests use test/framework/resources.go which builds unstructured objects via the dynamic client.
package utils

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Test Resource Builders - RuleSet
// -----------------------------------------------------------------------------

// RuleSetOptions provides options for creating test RuleSet resources
type RuleSetOptions struct {
	Name      string
	Namespace string
	Rules     []wafv1alpha1.RuleSourceReference
	RuleData  string
}

// NewTestRuleSet creates a test RuleSet resource with sensible defaults
func NewTestRuleSet(opts RuleSetOptions) *wafv1alpha1.RuleSet {
	if opts.Name == "" {
		opts.Name = "test-ruleset"
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.Rules == nil {
		opts.Rules = []wafv1alpha1.RuleSourceReference{
			{Name: "test-rules"},
		}
	}

	ruleset := &wafv1alpha1.RuleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: wafv1alpha1.RuleSetSpec{
			Rules: opts.Rules,
		},
	}

	if opts.RuleData != "" {
		ruleset.Spec.RuleData = &opts.RuleData
	}

	return ruleset
}

// NewTestConfigMap creates a test ConfigMap with WAF rules
func NewTestConfigMap(name, namespace, rules string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string]string{
			"rules": rules,
		},
	}
}

// NewTestRuleData creates a test Secret with WAF rules data
func NewTestRuleData(name, namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: wafv1alpha1.RuleDataSecretType,
		Data: data,
	}
}

// -----------------------------------------------------------------------------
// Test Resource Builders - Engine
// -----------------------------------------------------------------------------

// EngineOptions provides options for creating test Engine resources
type EngineOptions struct {
	Name                 string
	Namespace            string
	RuleSetName          string
	WasmImage            string
	PollIntervalSeconds  int32
	WorkloadLabels       map[string]string
	IstioIntegrationMode wafv1alpha1.IstioIntegrationMode
	FailurePolicy        wafv1alpha1.FailurePolicy
}

// NewTestEngine creates a test Engine resource with sensible defaults
func NewTestEngine(opts EngineOptions) *wafv1alpha1.Engine {
	if opts.Name == "" {
		opts.Name = "test-engine"
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.RuleSetName == "" {
		opts.RuleSetName = "test-ruleset"
	}
	if opts.WasmImage == "" {
		opts.WasmImage = "oci://fake-registry.io/fake-image:latest"
	}
	if opts.PollIntervalSeconds == 0 {
		opts.PollIntervalSeconds = 5
	}
	if opts.WorkloadLabels == nil {
		opts.WorkloadLabels = map[string]string{"app": "gateway"}
	}
	if opts.IstioIntegrationMode == "" {
		opts.IstioIntegrationMode = wafv1alpha1.IstioIntegrationModeGateway
	}
	if opts.FailurePolicy == "" {
		opts.FailurePolicy = wafv1alpha1.FailurePolicyFail
	}

	return &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: wafv1alpha1.EngineSpec{
			RuleSet: wafv1alpha1.RuleSetReference{
				Name: opts.RuleSetName,
			},
			Driver: &wafv1alpha1.DriverConfig{
				Istio: &wafv1alpha1.IstioDriverConfig{
					Wasm: &wafv1alpha1.IstioWasmConfig{
						Image: opts.WasmImage,
						WorkloadSelector: &metav1.LabelSelector{
							MatchLabels: opts.WorkloadLabels,
						},
						Mode: &opts.IstioIntegrationMode,
						RuleSetCacheServer: &wafv1alpha1.RuleSetCacheServerConfig{
							PollIntervalSeconds: &opts.PollIntervalSeconds,
						},
					},
				},
			},
			FailurePolicy: &opts.FailurePolicy,
		},
	}
}
