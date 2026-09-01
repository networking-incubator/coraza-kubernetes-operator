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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

func TestEngineReconciler_BuildTelemetry(t *testing.T) {
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "telemetry-engine",
		Namespace:   "waf",
		GatewayName: "protected-gateway",
	})

	telemetry := buildTelemetry(engine, "custom-waf-log-collector")

	assert.Equal(t, "coraza-engine-telemetry-engine-telemetry", telemetry.GetName())
	assert.Equal(t, "waf", telemetry.GetNamespace())
	assert.Equal(t, "telemetry-engine", telemetry.GetLabels()[engineNameLabel])

	spec, found, err := getNestedMap(telemetry.Object, "spec")
	require.NoError(t, err)
	require.True(t, found)
	selectorSpec, found, err := getNestedMap(spec, "selector")
	require.NoError(t, err)
	require.True(t, found)
	selector, found, err := getNestedMap(selectorSpec, "matchLabels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "protected-gateway", selector[gatewayNameLabel])

	providers, found, err := unstructured.NestedSlice(spec, "accessLogging")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, providers, 1)
	entry := providers[0].(map[string]any)
	providerRefs := entry["providers"].([]any)
	assert.Equal(t, "custom-waf-log-collector", providerRefs[0].(map[string]any)["name"])
}

func TestGatewayClassWAFCollectorProvider(t *testing.T) {
	class := &gatewayv1.GatewayClass{}
	class.SetAnnotations(map[string]string{wafCollectorAnnotation: "collector.example.svc.cluster.local:4317"})
	provider, found := gatewayClassWAFCollectorProvider(class)
	assert.True(t, found)
	assert.Equal(t, "waf-log-collector", provider)

	class.SetAnnotations(map[string]string{wafCollectorAnnotation: ""})
	_, found = gatewayClassWAFCollectorProvider(class)
	assert.False(t, found)
	_, found = gatewayClassWAFCollectorProvider(&gatewayv1.GatewayClass{})
	assert.False(t, found)
}

func TestEngineReconciler_ReconcileTelemetryCreatesAndRemovesTelemetry(t *testing.T) {
	ctx := context.Background()
	const namespace = "default"
	class := &gatewayv1.GatewayClass{}
	class.Name = "telemetry-test-class"
	class.Spec.ControllerName = "example.com/gateway-controller"
	class.Annotations = map[string]string{wafCollectorAnnotation: "collector.example.svc.cluster.local:4317"}
	require.NoError(t, k8sClient.Create(ctx, class))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, class) })

	gateway := createTestGateway(t, ctx, k8sClient, "telemetry-test-gateway", namespace)
	gateway.Spec.GatewayClassName = gatewayv1.ObjectName(class.Name)
	require.NoError(t, k8sClient.Update(ctx, gateway))

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "telemetry-test-engine",
		Namespace:   namespace,
		GatewayName: gateway.Name,
	})
	engine.Spec.Observability.Mode = wafv1alpha1.ObservabilityModeEnabled
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, engine) })

	reconciler := &EngineReconciler{Client: k8sClient, Scheme: scheme}
	require.NoError(t, reconciler.reconcileTelemetry(ctx, engine))

	telemetry := &unstructured.Unstructured{}
	telemetry.SetGroupVersionKind(telemetryGVK)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: telemetryName(engine.Name)}, telemetry))
	assert.Equal(t, engine.Name, telemetry.GetLabels()[engineNameLabel])
	accessLogging, found, err := unstructured.NestedSlice(telemetry.Object, "spec", "accessLogging")
	require.NoError(t, err)
	assert.True(t, found)
	require.Len(t, accessLogging, 1)
	providerRefs := accessLogging[0].(map[string]any)["providers"].([]any)
	require.Len(t, providerRefs, 1)
	assert.Equal(t, "waf-log-collector", providerRefs[0].(map[string]any)["name"])

	engine.Spec.Observability.Mode = wafv1alpha1.ObservabilityModeDisabled
	require.NoError(t, reconciler.reconcileTelemetry(ctx, engine))
	err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: telemetry.GetName()}, telemetry)
	assert.True(t, apierrors.IsNotFound(err), "expected Telemetry to be removed, got: %v", err)

	engine.Spec.Observability.Mode = wafv1alpha1.ObservabilityModeEnabled
	require.NoError(t, reconciler.reconcileTelemetry(ctx, engine))
	class.SetAnnotations(nil)
	require.NoError(t, k8sClient.Update(ctx, class))
	require.Error(t, reconciler.reconcileTelemetry(ctx, engine))
	err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: telemetry.GetName()}, telemetry)
	assert.True(t, apierrors.IsNotFound(err), "expected Telemetry to be removed when the collector declaration is absent, got: %v", err)
}
