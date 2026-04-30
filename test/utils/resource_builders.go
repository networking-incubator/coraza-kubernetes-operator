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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/defaults"
)

const (
	defaultTestRuleSetName = "test-ruleset"
	defaultTestNamespace   = "default"
)

// -----------------------------------------------------------------------------
// Test Resource Builders - RuleSource
// -----------------------------------------------------------------------------

// NewTestRuleSource creates a test RuleSource with the given rules text.
func NewTestRuleSource(name, namespace, rules string) *wafv1alpha1.RuleSource {
	return &wafv1alpha1.RuleSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: wafv1alpha1.RuleSourceSpec{
			Rules: rules,
		},
	}
}

// -----------------------------------------------------------------------------
// Test Resource Builders - RuleData
// -----------------------------------------------------------------------------

// NewTestRuleData creates a test RuleData with the given files.
func NewTestRuleData(name, namespace string, files map[string]string) *wafv1alpha1.RuleData {
	return &wafv1alpha1.RuleData{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: wafv1alpha1.RuleDataSpec{
			Files: files,
		},
	}
}

// -----------------------------------------------------------------------------
// Test Resource Builders - RuleSet
// -----------------------------------------------------------------------------

// RuleSetOptions provides options for creating test RuleSet resources
type RuleSetOptions struct {
	Name      string
	Namespace string
	Sources   []wafv1alpha1.SourceReference
	Data      []wafv1alpha1.DataReference
}

// NewTestRuleSet creates a test RuleSet resource with sensible defaults
func NewTestRuleSet(opts RuleSetOptions) *wafv1alpha1.RuleSet {
	if opts.Name == "" {
		opts.Name = defaultTestRuleSetName
	}
	if opts.Namespace == "" {
		opts.Namespace = defaultTestNamespace
	}
	if opts.Sources == nil {
		opts.Sources = []wafv1alpha1.SourceReference{
			{Name: "test-rules"},
		}
	}

	ruleset := &wafv1alpha1.RuleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: wafv1alpha1.RuleSetSpec{
			Sources: opts.Sources,
			Data:    opts.Data,
		},
	}

	return ruleset
}

// -----------------------------------------------------------------------------
// Test Resource Builders - Engine
// -----------------------------------------------------------------------------

// EngineOptions provides options for creating test Engine resources
type EngineOptions struct {
	Name                string
	Namespace           string
	RuleSetName         string
	WasmImage           string
	ImagePullSecret     string
	PollIntervalSeconds int32
	GatewayName         string
	FailurePolicy       wafv1alpha1.FailurePolicy
}

// NewTestEngine creates a test Engine resource with sensible defaults
func NewTestEngine(opts EngineOptions) *wafv1alpha1.Engine {
	if opts.Name == "" {
		opts.Name = "test-engine"
	}
	if opts.Namespace == "" {
		opts.Namespace = defaultTestNamespace
	}
	if opts.RuleSetName == "" {
		opts.RuleSetName = defaultTestRuleSetName
	}
	if opts.WasmImage == "" {
		opts.WasmImage = defaults.DefaultCorazaWasmOCIReference
	}
	if opts.PollIntervalSeconds == 0 {
		opts.PollIntervalSeconds = 5
	}
	if opts.GatewayName == "" {
		opts.GatewayName = "test-gw"
	}
	if opts.FailurePolicy == "" {
		opts.FailurePolicy = wafv1alpha1.FailurePolicyFail
	}

	engine := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: wafv1alpha1.EngineSpec{
			RuleSet: wafv1alpha1.RuleSetReference{
				Name: opts.RuleSetName,
			},
			Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: opts.GatewayName,
			},
			RuleSetCacheServer: &wafv1alpha1.RuleSetCacheServerConfig{
				PollIntervalSeconds: opts.PollIntervalSeconds,
			},
			Driver: wafv1alpha1.DriverConfig{
				Type: wafv1alpha1.DriverTypeWasm,
				Wasm: &wafv1alpha1.WasmDriverConfig{
					Image: opts.WasmImage,
				},
			},
			FailurePolicy: opts.FailurePolicy,
		},
	}

	if opts.ImagePullSecret != "" {
		engine.Spec.Driver.Wasm.ImagePullSecret = opts.ImagePullSecret
	}

	return engine
}

// -----------------------------------------------------------------------------
// Test Resource Builders - Engine (Dynamic Module)
// -----------------------------------------------------------------------------

// DynamicModuleEngineOptions provides options for creating test Engine resources
// with the dynamic module driver.
type DynamicModuleEngineOptions struct {
	Name        string
	Namespace   string
	RuleSetName string
	GatewayName string
	ModuleName  string
	FilterName  string
	FilterMode  wafv1alpha1.DynamicModuleFilterMode
	ModuleImage string
	ProxyImage  string
}

// NewTestDynamicModuleEngine creates a test Engine resource configured with the
// dynamic module driver.
func NewTestDynamicModuleEngine(opts DynamicModuleEngineOptions) *wafv1alpha1.Engine {
	if opts.Name == "" {
		opts.Name = "test-engine"
	}
	if opts.Namespace == "" {
		opts.Namespace = defaultTestNamespace
	}
	if opts.RuleSetName == "" {
		opts.RuleSetName = defaultTestRuleSetName
	}
	if opts.GatewayName == "" {
		opts.GatewayName = "test-gw"
	}
	if opts.ModuleName == "" {
		opts.ModuleName = "composer"
	}
	if opts.FilterName == "" {
		opts.FilterName = "coraza-waf"
	}
	if opts.FilterMode == "" {
		opts.FilterMode = wafv1alpha1.DynamicModuleFilterModeFull
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
			Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: opts.GatewayName,
			},
			Driver: wafv1alpha1.DriverConfig{
				Type: wafv1alpha1.DriverTypeDynamicModule,
				DynamicModule: &wafv1alpha1.DynamicModuleDriverConfig{
					ModuleName:  opts.ModuleName,
					FilterName:  opts.FilterName,
					FilterMode:  opts.FilterMode,
					ModuleImage: opts.ModuleImage,
					ProxyImage:  opts.ProxyImage,
				},
			},
			FailurePolicy: wafv1alpha1.FailurePolicyFail,
		},
	}
}
