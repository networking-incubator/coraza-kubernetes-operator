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
	"strconv"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Engine Controller - PodMonitor RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=podmonitors,verbs=get;list;watch;create;update;patch;delete

// -----------------------------------------------------------------------------
// Engine Controller - PodMonitor Constants
// -----------------------------------------------------------------------------

const (
	podMonitorNameSuffix = "-dataplane"

	defaultPodMonitorPortName      = "15090"
	defaultPodMonitorInterval      = "30s"
	defaultPodMonitorScrapeTimeout = "10s"
)

// -----------------------------------------------------------------------------
// Engine Controller - PodMonitor Naming
// -----------------------------------------------------------------------------

func podMonitorName(engineName string) string {
	return wasmPluginName(engineName) + podMonitorNameSuffix
}

func podMonitorGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "PodMonitor",
	}
}

// engineDataplaneResourceLabels returns labels used to associate dataplane
// resources (PodMonitor) with an Engine.
func engineDataplaneResourceLabels(engine *wafv1alpha1.Engine) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":    "coraza-kubernetes-operator",
		networkPolicyEngineLabelName:      engine.Name,
		networkPolicyEngineLabelNamespace: engine.Namespace,
	}
}

// -----------------------------------------------------------------------------
// Engine Controller - PodMonitor Apply / Delete
// -----------------------------------------------------------------------------

func (r *EngineReconciler) applyPodMonitor(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) error {
	if !r.dataplanePodMonitorEnabled {
		return nil
	}
	if !hasGatewayTarget(engine) {
		return nil
	}
	if !r.podMonitorCRDAvailable {
		logInfo(log, req, "Engine", "Skipping PodMonitor: monitoring.coreos.com/v1 PodMonitor CRD not installed")
		return nil
	}

	ws := targetLabelSelector(engine)
	if ws == nil || len(ws.MatchLabels) == 0 {
		return fmt.Errorf("cannot derive gateway pod selector for PodMonitor")
	}

	desired := r.buildPodMonitor(engine, ws.MatchLabels)

	if err := controllerutil.SetControllerReference(engine, desired, r.Scheme); err != nil {
		return fmt.Errorf("set PodMonitor owner reference: %w", err)
	}

	logDebug(log, req, "Engine", "Applying PodMonitor", "podMonitorName", desired.GetName())
	if err := serverSideApply(ctx, r.Client, desired); err != nil {
		return fmt.Errorf("apply PodMonitor: %w", err)
	}

	logInfo(log, req, "Engine", "PodMonitor provisioned",
		"podMonitorNamespace", desired.GetNamespace(),
		"podMonitorName", desired.GetName())
	return nil
}

func (r *EngineReconciler) cleanupPodMonitor(ctx context.Context, log logr.Logger, req ctrl.Request) error {
	if !r.dataplanePodMonitorEnabled || !r.podMonitorCRDAvailable {
		return nil
	}

	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(podMonitorGVK())
	name := podMonitorName(req.Name)
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: req.Namespace}, pm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get PodMonitor for cleanup: %w", err)
	}

	if delErr := r.Delete(ctx, pm); client.IgnoreNotFound(delErr) != nil {
		logAPIError(log, req, "Engine", delErr, "Failed to delete PodMonitor", pm)
		return delErr
	}
	logInfo(log, req, "Engine", "Deleted PodMonitor", "podMonitor", name)
	return nil
}

// -----------------------------------------------------------------------------
// Engine Controller - PodMonitor Builder
// -----------------------------------------------------------------------------

func (r *EngineReconciler) buildPodMonitor(engine *wafv1alpha1.Engine, matchLabels map[string]string) *unstructured.Unstructured {
	labels := engineDataplaneResourceLabels(engine)
	for k, v := range r.dataplanePodMonitorLabels {
		labels[k] = v
	}

	interval := r.dataplanePodMonitorInterval
	if interval == "" {
		interval = defaultPodMonitorInterval
	}
	scrapeTimeout := r.dataplanePodMonitorScrapeTimeout
	if scrapeTimeout == "" {
		scrapeTimeout = defaultPodMonitorScrapeTimeout
	}
	portName := r.dataplanePodMonitorPortName
	if portName == "" {
		portName = defaultPodMonitorPortName
	}

	matchLabelsAny := make(map[string]any, len(matchLabels))
	for k, v := range matchLabels {
		matchLabelsAny[k] = v
	}

	relabelings := dataplanePodMonitorMetricRelabelings()
	relabelingsAny := make([]any, len(relabelings))
	for i, rule := range relabelings {
		relabelingsAny[i] = rule
	}
	endpoint := map[string]any{
		"path":              "/stats/prometheus",
		"interval":          interval,
		"scrapeTimeout":     scrapeTimeout,
		"metricRelabelings": relabelingsAny,
	}
	if num, err := strconv.ParseInt(portName, 10, 64); err == nil {
		endpoint["targetPort"] = num
	} else {
		endpoint["port"] = portName
	}

	pm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "monitoring.coreos.com/v1",
			"kind":       "PodMonitor",
			"metadata": map[string]any{
				"name":      podMonitorName(engine.Name),
				"namespace": engine.Namespace,
			},
			"spec": map[string]any{
				"namespaceSelector": map[string]any{
					"any": true,
				},
				"selector": map[string]any{
					"matchLabels": matchLabelsAny,
				},
				"podMetricsEndpoints": []any{endpoint},
			},
		},
	}
	pm.SetGroupVersionKind(podMonitorGVK())
	pm.SetLabels(labels)
	pm.SetName(podMonitorName(engine.Name))
	pm.SetNamespace(engine.Namespace)
	return pm
}
