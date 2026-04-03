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
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestBuildServiceEntry_Shape(t *testing.T) {
	p := newTestPrereqs("my-op", "test-ns", "")
	labels := map[string]string{
		"app.kubernetes.io/name":     "my-op",
		"app.kubernetes.io/instance": "my-op",
	}
	se := p.buildServiceEntry("my-op-ruleset-cache", "my-op.test-ns.svc.cluster.local", labels, testOwnerRef())

	gvk := se.GetObjectKind().GroupVersionKind()
	assert.Equal(t, "networking.istio.io", gvk.Group)
	assert.Equal(t, "v1", gvk.Version)
	assert.Equal(t, "ServiceEntry", gvk.Kind)

	assert.Equal(t, "my-op-ruleset-cache", se.GetName())
	assert.Equal(t, "test-ns", se.GetNamespace())
	assert.Equal(t, labels, se.GetLabels())

	refs := se.GetOwnerReferences()
	require.Len(t, refs, 1)
	assert.Equal(t, "Deployment", refs[0].Kind)
	assert.Equal(t, "my-op", refs[0].Name)
	assert.Equal(t, types.UID("aaaa-bbbb"), refs[0].UID)
	assert.Nil(t, refs[0].BlockOwnerDeletion)

	spec, ok := se.Object["spec"].(map[string]any)
	require.True(t, ok, "spec should be map[string]any")

	hosts, _ := spec["hosts"].([]any)
	require.Len(t, hosts, 1)
	assert.Equal(t, "my-op.test-ns.svc.cluster.local", hosts[0])

	assert.Equal(t, "MESH_INTERNAL", spec["location"])
	assert.Equal(t, "DNS", spec["resolution"])

	endpoints, _ := spec["endpoints"].([]any)
	require.Len(t, endpoints, 1)
	ep, _ := endpoints[0].(map[string]any)
	assert.Equal(t, "my-op.test-ns.svc.cluster.local", ep["address"])

	ports, _ := spec["ports"].([]any)
	require.Len(t, ports, 1)
	port, _ := ports[0].(map[string]any)
	assert.Equal(t, int64(443), port["number"])
	assert.Equal(t, "https-ruleset-cache-server", port["name"])
	assert.Equal(t, "HTTPS", port["protocol"])
}

func TestBuildDestinationRule_Shape(t *testing.T) {
	p := newTestPrereqs("my-op", "test-ns", "")
	labels := map[string]string{
		"app.kubernetes.io/name":     "my-op",
		"app.kubernetes.io/instance": "my-op",
	}
	dr := p.buildDestinationRule("my-op-ruleset-cache", "my-op.test-ns.svc.cluster.local", labels, testOwnerRef())

	gvk := dr.GetObjectKind().GroupVersionKind()
	assert.Equal(t, "networking.istio.io", gvk.Group)
	assert.Equal(t, "v1", gvk.Version)
	assert.Equal(t, "DestinationRule", gvk.Kind)

	assert.Equal(t, "my-op-ruleset-cache", dr.GetName())
	assert.Equal(t, "test-ns", dr.GetNamespace())
	assert.Equal(t, labels, dr.GetLabels())

	refs := dr.GetOwnerReferences()
	require.Len(t, refs, 1)
	assert.Equal(t, "Deployment", refs[0].Kind)

	spec, ok := dr.Object["spec"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, "my-op.test-ns.svc.cluster.local", spec["host"])

	tp, _ := spec["trafficPolicy"].(map[string]any)
	require.NotNil(t, tp, "trafficPolicy should be present")
	tls, _ := tp["tls"].(map[string]any)
	require.NotNil(t, tls, "tls should be present")
	assert.Equal(t, "SIMPLE", tls["mode"])
	assert.Equal(t, "cko-ca-credential", tls["credentialName"])
}

func TestNewIstioObject_IstioRevisionLabel(t *testing.T) {
	t.Run("without revision", func(t *testing.T) {
		p := newTestPrereqs("op", "ns", "")
		labels := map[string]string{
			"app.kubernetes.io/name":     "op",
			"app.kubernetes.io/instance": "op",
		}
		obj := p.newIstioObject("ServiceEntry", "test", labels, testOwnerRef(), map[string]any{})
		_, hasRev := obj.GetLabels()["istio.io/rev"]
		assert.False(t, hasRev, "istio.io/rev should not be set when revision is empty")
	})

	t.Run("with revision", func(t *testing.T) {
		p := newTestPrereqs("op", "ns", "canary")
		labels := map[string]string{
			"app.kubernetes.io/name":     "op",
			"app.kubernetes.io/instance": "op",
			"istio.io/rev":               "canary",
		}
		obj := p.newIstioObject("ServiceEntry", "test", labels, testOwnerRef(), map[string]any{})
		assert.Equal(t, "canary", obj.GetLabels()["istio.io/rev"])
	})
}

func TestIstioPrerequisites_Apply(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)
	deploy := createDeployment(t, ctx, "test-op", namespace)

	p := NewIstioPrerequisites(k8sClient, k8sClient, "test-op", namespace, "")
	log := ctrl.Log.WithName("test")
	require.NoError(t, p.apply(ctx, log))

	resourceName := "test-op-ruleset-cache"

	se := &unstructured.Unstructured{}
	se.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "networking.istio.io", Version: "v1", Kind: "ServiceEntry",
	})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name: resourceName, Namespace: namespace,
	}, se))

	assert.Equal(t, "test-op", se.GetLabels()["app.kubernetes.io/name"])
	seRefs := se.GetOwnerReferences()
	require.Len(t, seRefs, 1)
	assert.Equal(t, deploy.UID, seRefs[0].UID)

	dr := &unstructured.Unstructured{}
	dr.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule",
	})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name: resourceName, Namespace: namespace,
	}, dr))

	assert.Equal(t, "test-op", dr.GetLabels()["app.kubernetes.io/name"])
	drRefs := dr.GetOwnerReferences()
	require.Len(t, drRefs, 1)
	assert.Equal(t, deploy.UID, drRefs[0].UID)
}

func TestIstioPrerequisites_ApplyIdempotent(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)
	createDeployment(t, ctx, "test-op-idem", namespace)

	p := NewIstioPrerequisites(k8sClient, k8sClient, "test-op-idem", namespace, "")
	log := ctrl.Log.WithName("test-idempotent")

	require.NoError(t, p.apply(ctx, log), "first apply")
	require.NoError(t, p.apply(ctx, log), "second apply should succeed (idempotent)")
}

func TestIstioPrerequisites_ApplyWithIstioRevision(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)
	createDeployment(t, ctx, "test-op-rev", namespace)

	p := NewIstioPrerequisites(k8sClient, k8sClient, "test-op-rev", namespace, "canary")
	log := ctrl.Log.WithName("test-revision")
	require.NoError(t, p.apply(ctx, log))

	se := &unstructured.Unstructured{}
	se.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "networking.istio.io", Version: "v1", Kind: "ServiceEntry",
	})
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name: "test-op-rev-ruleset-cache", Namespace: namespace,
	}, se))
	assert.Equal(t, "canary", se.GetLabels()["istio.io/rev"])
}

func TestIstioPrerequisites_ApplyDeploymentNotFound(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	p := NewIstioPrerequisites(k8sClient, k8sClient, "no-such-deploy", namespace, "")
	log := ctrl.Log.WithName("test-not-found")
	err := p.apply(ctx, log)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "looking up owner Deployment")
}

func TestIstioPrerequisites_StartReturnsNilOnError(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	p := NewIstioPrerequisites(k8sClient, k8sClient, "no-such-deploy", namespace, "")
	err := p.Start(ctx)
	assert.NoError(t, err, "Start() must return nil even when apply fails")
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func newTestPrereqs(operatorName, namespace, istioRevision string) *IstioPrerequisites {
	return &IstioPrerequisites{
		operatorName:  operatorName,
		namespace:     namespace,
		istioRevision: istioRevision,
	}
}

func testOwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "my-op",
		UID:        types.UID("aaaa-bbbb"),
	}
}

// setupTestNamespace creates an isolated namespace and returns its name.
func setupTestNamespace(t *testing.T, ctx context.Context) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("envtest-%s", uuid.New().String()),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ns))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ns); err != nil {
			t.Logf("Failed to delete test namespace: %v", err)
		}
	})
	return ns.Name
}

func createDeployment(t *testing.T, ctx context.Context, name, namespace string) *appsv1.Deployment {
	t.Helper()
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "controller",
						Image: "ghcr.io/test:latest",
					}},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, deploy))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, deploy); err != nil {
			t.Logf("Failed to delete test deployment: %v", err)
		}
	})
	return deploy
}
