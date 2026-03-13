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

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries;destinationrules,verbs=get;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get

// IstioPrerequisites creates the Istio ServiceEntry and DestinationRule
// resources required for the RuleSet cache server to be reachable from
// Envoy sidecars within the mesh. These resources are applied once at
// startup using server-side apply.
type IstioPrerequisites struct {
	client        client.Client
	operatorName  string
	namespace     string
	istioRevision string
}

// NewIstioPrerequisites returns a new IstioPrerequisites runnable.
func NewIstioPrerequisites(c client.Client, operatorName, namespace, istioRevision string) *IstioPrerequisites {
	return &IstioPrerequisites{
		client:        c,
		operatorName:  operatorName,
		namespace:     namespace,
		istioRevision: istioRevision,
	}
}

// Start applies the Istio ServiceEntry and DestinationRule for the
// RuleSet cache server. It satisfies the manager.Runnable interface.
func (p *IstioPrerequisites) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("istio-prerequisites")

	var deploy appsv1.Deployment
	if err := p.client.Get(ctx, types.NamespacedName{Name: p.operatorName, Namespace: p.namespace}, &deploy); err != nil {
		return fmt.Errorf("looking up owner Deployment %s/%s: %w", p.namespace, p.operatorName, err)
	}
	ownerRef := metav1.OwnerReference{
		APIVersion:         "apps/v1",
		Kind:               "Deployment",
		Name:               deploy.Name,
		UID:                deploy.UID,
		BlockOwnerDeletion: boolPtr(true),
	}

	serviceFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", p.operatorName, p.namespace)
	resourceName := p.operatorName + "-ruleset-cache"

	labels := map[string]any{
		"app.kubernetes.io/name":     p.operatorName,
		"app.kubernetes.io/instance": p.operatorName,
	}
	if p.istioRevision != "" {
		labels["istio.io/rev"] = p.istioRevision
	}

	se := p.buildServiceEntry(resourceName, serviceFQDN, labels, ownerRef)
	log.Info("Applying ServiceEntry", "name", resourceName, "namespace", p.namespace)
	if err := serverSideApply(ctx, p.client, se); err != nil {
		return fmt.Errorf("applying Istio ServiceEntry: %w", err)
	}

	dr := p.buildDestinationRule(resourceName, serviceFQDN, labels, ownerRef)
	log.Info("Applying DestinationRule", "name", resourceName, "namespace", p.namespace)
	if err := serverSideApply(ctx, p.client, dr); err != nil {
		return fmt.Errorf("applying Istio DestinationRule: %w", err)
	}

	return nil
}

func boolPtr(b bool) *bool { return &b }

func (p *IstioPrerequisites) buildServiceEntry(name, serviceFQDN string, labels map[string]any, ownerRef metav1.OwnerReference) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "networking.istio.io/v1",
			"kind":       "ServiceEntry",
			"metadata": map[string]any{
				"name":      name,
				"namespace": p.namespace,
				"labels":    labels,
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         ownerRef.APIVersion,
						"kind":               ownerRef.Kind,
						"name":               ownerRef.Name,
						"uid":                string(ownerRef.UID),
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"hosts": []any{serviceFQDN},
				"ports": []any{
					map[string]any{
						"number":   int64(80),
						"name":     "http",
						"protocol": "HTTP",
					},
				},
				"location":   "MESH_INTERNAL",
				"resolution": "DNS",
				"endpoints": []any{
					map[string]any{
						"address": serviceFQDN,
					},
				},
			},
		},
	}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.istio.io",
		Version: "v1",
		Kind:    "ServiceEntry",
	})
	return obj
}

func (p *IstioPrerequisites) buildDestinationRule(name, serviceFQDN string, labels map[string]any, ownerRef metav1.OwnerReference) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "networking.istio.io/v1",
			"kind":       "DestinationRule",
			"metadata": map[string]any{
				"name":      name,
				"namespace": p.namespace,
				"labels":    labels,
				"ownerReferences": []any{
					map[string]any{
						"apiVersion":         ownerRef.APIVersion,
						"kind":               ownerRef.Kind,
						"name":               ownerRef.Name,
						"uid":                string(ownerRef.UID),
						"blockOwnerDeletion": true,
					},
				},
			},
			"spec": map[string]any{
				"host": serviceFQDN,
				"trafficPolicy": map[string]any{
					"tls": map[string]any{
						"mode": "DISABLE",
					},
				},
			},
		},
	}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.istio.io",
		Version: "v1",
		Kind:    "DestinationRule",
	})
	return obj
}
