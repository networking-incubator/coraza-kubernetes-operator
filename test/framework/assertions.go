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

package framework

import (
	"fmt"
	"strings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// -----------------------------------------------------------------------------
// Condition Assertions
// -----------------------------------------------------------------------------

// ExpectCondition polls until the named resource has the specified condition
// type with the expected status value ("True", "False", or "Unknown").
// On timeout, the failure message includes the last-observed conditions.
func (s *Scenario) ExpectCondition(namespace, name string, gvr schema.GroupVersionResource, condType, status string) {
	s.T.Helper()
	require.EventuallyWithT(s.T, func(collect *assert.CollectT) {
		obj, err := s.F.DynamicClient.Resource(gvr).Namespace(namespace).Get(
			s.T.Context(), name, metav1.GetOptions{},
		)
		if !assert.NoError(collect, err, "get %s %s/%s", gvr.Resource, namespace, name) {
			return
		}
		assert.True(collect, hasCondition(obj, condType, status),
			"%s %s/%s: expected condition %s=%s, got: [%s]",
			gvr.Resource, namespace, name, condType, status, formatConditions(obj),
		)
	}, DefaultTimeout, DefaultInterval)
}

// ExpectEngineReady polls until the Engine has condition Ready=True.
func (s *Scenario) ExpectEngineReady(namespace, name string) {
	s.T.Helper()
	s.T.Logf("Waiting for Engine %s/%s to be Ready", namespace, name)
	s.ExpectCondition(namespace, name, EngineGVR, "Ready", "True")
}

// ExpectEngineDegraded polls until the Engine has condition Degraded=True.
func (s *Scenario) ExpectEngineDegraded(namespace, name string) {
	s.T.Helper()
	s.T.Logf("Waiting for Engine %s/%s to be Degraded", namespace, name)
	s.ExpectCondition(namespace, name, EngineGVR, "Degraded", "True")
}

// ExpectRuleSetReady polls until the RuleSet has condition Ready=True.
func (s *Scenario) ExpectRuleSetReady(namespace, name string) {
	s.T.Helper()
	s.T.Logf("Waiting for Rule Set %s/%s to be Ready", namespace, name)
	s.ExpectCondition(namespace, name, RuleSetGVR, "Ready", "True")
}

// ExpectRuleSetDegraded polls until the RuleSet has condition Degraded=True.
func (s *Scenario) ExpectRuleSetDegraded(namespace, name string) {
	s.T.Helper()
	s.T.Logf("Waiting for RuleSet %s/%s to be Degraded", namespace, name)
	s.ExpectCondition(namespace, name, RuleSetGVR, "Degraded", "True")
}

// ExpectGatewayProgrammed polls until the Gateway has condition
// Programmed=True.
func (s *Scenario) ExpectGatewayProgrammed(namespace, name string) {
	s.T.Helper()
	s.T.Logf("Waiting for Gateway %s/%s to be Programmed", namespace, name)
	s.ExpectCondition(namespace, name, GatewayGVR, "Programmed", "True")
}

// ExpectGatewayAccepted polls until the Gateway has condition Accepted=True.
func (s *Scenario) ExpectGatewayAccepted(namespace, name string) {
	s.T.Helper()
	s.T.Logf("Waiting for Gateway %s/%s to be Accepted", namespace, name)
	s.ExpectCondition(namespace, name, GatewayGVR, "Accepted", "True")
}

// ExpectEngineGateways polls until the Engine's status.gateways matches the
// expected list of Gateway names. Pass an empty slice for engines with no
// matching gateways.
func (s *Scenario) ExpectEngineGateways(namespace, name string, expectedNames []string) {
	s.T.Helper()
	s.T.Logf("Waiting for Engine %s/%s to have gateways: %v", namespace, name, expectedNames)
	require.EventuallyWithT(s.T, func(collect *assert.CollectT) {
		obj, err := s.F.DynamicClient.Resource(EngineGVR).Namespace(namespace).Get(
			s.T.Context(), name, metav1.GetOptions{},
		)
		if !assert.NoError(collect, err, "get Engine %s/%s", namespace, name) {
			return
		}
		gateways, _, _ := unstructured.NestedSlice(obj.Object, "status", "gateways")
		var gotNames []string
		for _, g := range gateways {
			gw, ok := g.(map[string]any)
			if !ok {
				continue
			}
			if n, ok := gw["name"].(string); ok {
				gotNames = append(gotNames, n)
			}
		}
		assert.Equal(collect, expectedNames, gotNames,
			"Engine %s/%s: expected gateways %v, got %v", namespace, name, expectedNames, gotNames)
	}, DefaultTimeout, DefaultInterval)
}

// -----------------------------------------------------------------------------
// Resource Existence Assertions
// -----------------------------------------------------------------------------

// ExpectWasmPluginExists polls until a WasmPlugin with the given name exists
// in the namespace.
func (s *Scenario) ExpectWasmPluginExists(namespace, name string) {
	s.T.Helper()
	s.T.Logf("Waiting for WasmPlugin %s/%s to exist", namespace, name)
	require.Eventually(s.T, func() bool {
		_, err := s.F.DynamicClient.Resource(WasmPluginGVR).Namespace(namespace).Get(
			s.T.Context(), name, metav1.GetOptions{},
		)
		return err == nil
	}, DefaultTimeout, DefaultInterval, "WasmPlugin %s/%s should exist", namespace, name)
}

// ExpectResourceGone polls until the specified resource no longer exists.
func (s *Scenario) ExpectResourceGone(namespace, name string, gvr schema.GroupVersionResource) {
	s.T.Helper()
	s.T.Logf("Waiting for %s %s/%s to be deleted", gvr.Resource, namespace, name)
	require.Eventually(s.T, func() bool {
		_, err := s.F.DynamicClient.Resource(gvr).Namespace(namespace).Get(
			s.T.Context(), name, metav1.GetOptions{},
		)
		return apierrors.IsNotFound(err)
	}, DefaultTimeout, DefaultInterval,
		"%s %s/%s should not exist", gvr.Resource, namespace, name,
	)
}

// -----------------------------------------------------------------------------
// Validation Assertions
// -----------------------------------------------------------------------------

// ExpectCreateFails asserts that fn returns an error whose message contains
// the given substring. Use this to test CRD validation rejection.
func (s *Scenario) ExpectCreateFails(msg string, fn func() error) {
	s.T.Helper()
	err := fn()
	require.Error(s.T, err, "expected creation to fail")
	require.Contains(s.T, err.Error(), msg,
		"expected error to contain %q, got: %v", msg, err,
	)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func hasCondition(obj *unstructured.Unstructured, condType, status string) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == condType && cond["status"] == status {
			return true
		}
	}
	return false
}

func formatConditions(obj *unstructured.Unstructured) string {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return "no conditions"
	}
	parts := make([]string, 0, len(conditions))
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", cond["type"], cond["status"]))
	}
	if len(parts) == 0 {
		return "no conditions"
	}
	return strings.Join(parts, ", ")
}
