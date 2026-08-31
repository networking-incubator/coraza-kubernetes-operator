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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

const (
	// wafCollectorAnnotation is set by the GatewayClass controller to name the
	// Istio extension provider used by the shared WAF access-log collector.
	wafCollectorAnnotation = "internal.do-not-use.openshift.io/waf-otel-collector"

	telemetryNameSuffix = "-telemetry"
	engineNameLabel     = "waf.k8s.coraza.io/engine"
)

var telemetryGVK = schema.GroupVersionKind{
	Group: "telemetry.istio.io", Version: "v1", Kind: "Telemetry",
}

func telemetryName(engineName string) string {
	return wasmPluginName(engineName) + telemetryNameSuffix
}

// reconcileTelemetry creates the Gateway-scoped Istio Telemetry requested by
// an observability-enabled Engine. The GatewayClass owns collector endpoint
// registration; this controller only consumes that declaration and selects the
// corresponding mesh extension provider.
func (r *EngineReconciler) reconcileTelemetry(ctx context.Context, engine *wafv1alpha1.Engine) error {
	telemetry := &unstructured.Unstructured{}
	telemetry.SetGroupVersionKind(telemetryGVK)
	telemetry.SetNamespace(engine.Namespace)
	telemetry.SetName(telemetryName(engine.Name))

	if !observabilityEnabled(engine) {
		return r.deleteTelemetry(ctx, telemetry)
	}

	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: engine.Spec.Target.Name}, &gateway); err != nil {
		return fmt.Errorf("get target Gateway: %w", err)
	}

	var gatewayClass gatewayv1.GatewayClass
	if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		return fmt.Errorf("get GatewayClass %q: %w", gateway.Spec.GatewayClassName, err)
	}
	providerName, hasCollector := gatewayClassWAFCollectorProvider(&gatewayClass)
	if !hasCollector {
		if err := r.deleteTelemetry(ctx, telemetry); err != nil {
			return err
		}
		return fmt.Errorf("GatewayClass %q does not declare %q", gatewayClass.Name, wafCollectorAnnotation)
	}

	telemetry = buildTelemetry(engine, providerName)
	if err := controllerutil.SetControllerReference(engine, telemetry, r.Scheme); err != nil {
		return fmt.Errorf("set Telemetry owner reference: %w", err)
	}
	if err := serverSideApply(ctx, r.Client, telemetry); err != nil {
		return fmt.Errorf("apply Telemetry: %w", err)
	}
	return nil
}

func (r *EngineReconciler) deleteTelemetry(ctx context.Context, telemetry *unstructured.Unstructured) error {
	if err := r.Delete(ctx, telemetry); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Telemetry: %w", err)
	}
	return nil
}

func buildTelemetry(engine *wafv1alpha1.Engine, providerName string) *unstructured.Unstructured {
	telemetry := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": telemetryGVK.GroupVersion().String(),
		"kind":       telemetryGVK.Kind,
		"metadata": map[string]any{
			"name":      telemetryName(engine.Name),
			"namespace": engine.Namespace,
			"labels": map[string]any{
				engineNameLabel: engine.Name,
			},
		},
		"spec": map[string]any{
			"selector": map[string]any{
				"matchLabels": map[string]any{gatewayNameLabel: engine.Spec.Target.Name},
			},
			"accessLogging": []any{map[string]any{
				"providers": []any{map[string]any{"name": providerName}},
			}},
		},
	}}
	telemetry.SetGroupVersionKind(telemetryGVK)
	return telemetry
}

func gatewayClassWAFCollectorProvider(gatewayClass *gatewayv1.GatewayClass) (string, bool) {
	if gatewayClass == nil {
		return "", false
	}
	providerName := gatewayClass.GetAnnotations()[wafCollectorAnnotation]
	return providerName, providerName != ""
}
