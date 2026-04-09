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
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/defaults"
)

// Resource builders, GVRs, and CRUD helpers for integration tests.
// CRD builders (Engine, RuleSet) use typed API objects from api/v1alpha1
// and convert to unstructured for the dynamic client. External resources
// (Gateway, HTTPRoute) are built as unstructured directly.

// gatewayMu serializes Gateway creation to reduce Istio CA certificate
// signing contention when many parallel tests create Gateways simultaneously.
// This prevents conflicts on istio-ca-root-cert ConfigMap updates.
var gatewayMu sync.Mutex

// -----------------------------------------------------------------------------
// GVRs
// -----------------------------------------------------------------------------

var (
	// EngineGVR is the GroupVersionResource for Engine resources.
	EngineGVR = schema.GroupVersionResource{
		Group: "waf.k8s.coraza.io", Version: "v1alpha1", Resource: "engines",
	}

	// RuleSetGVR is the GroupVersionResource for RuleSet resources.
	RuleSetGVR = schema.GroupVersionResource{
		Group: "waf.k8s.coraza.io", Version: "v1alpha1", Resource: "rulesets",
	}

	// GatewayGVR is the GroupVersionResource for Gateway resources.
	GatewayGVR = schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways",
	}

	// HTTPRouteGVR is the GroupVersionResource for HTTPRoute resources.
	HTTPRouteGVR = schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes",
	}

	// WasmPluginGVR is the GroupVersionResource for WasmPlugin resources.
	WasmPluginGVR = schema.GroupVersionResource{
		Group: "extensions.istio.io", Version: "v1alpha1", Resource: "wasmplugins",
	}
)

// -----------------------------------------------------------------------------
// Option Types
// -----------------------------------------------------------------------------

// EngineOpts configures an Engine resource for creation.
type EngineOpts struct {
	// RuleSetName is the name of the RuleSet to reference (required).
	RuleSetName string

	// GatewayName sets the workload selector to target this gateway's pods
	// via the gateway.networking.k8s.io/gateway-name label. Ignored if
	// WorkloadLabels or GatewayNames is set.
	GatewayName string

	// GatewayNames sets the workload selector to target pods from multiple
	// Gateways using a matchExpressions In selector on the
	// gateway.networking.k8s.io/gateway-name label. Takes precedence over
	// GatewayName and WorkloadLabels.
	GatewayNames []string

	// WorkloadLabels overrides the workload selector. Takes precedence over
	// GatewayName.
	WorkloadLabels map[string]string

	// WasmImage is the OCI image for the WASM plugin. Defaults to the
	// CORAZA_WASM_IMAGE env var, or a built-in default.
	WasmImage string

	// FailurePolicy determines behavior when the WAF is not ready.
	// Defaults to wafv1alpha1.FailurePolicyFail.
	FailurePolicy wafv1alpha1.FailurePolicy

	// PollInterval is the ruleSetCacheServer poll interval in seconds.
	// Defaults to 5.
	PollInterval int32
}

// -----------------------------------------------------------------------------
// Defaults
// -----------------------------------------------------------------------------

func defaultWasmImage() string {
	if img := os.Getenv("CORAZA_WASM_IMAGE"); img != "" {
		return img
	}
	return defaults.DefaultCorazaWasmOCIReference
}

const fallbackEchoImage = "registry.k8s.io/gateway-api/echo-basic:v20251204-v1.4.1"

func defaultEchoImage() string {
	if img := os.Getenv("ECHO_IMAGE"); img != "" {
		return img
	}
	return fallbackEchoImage
}

// SimpleBlockRule generates a SecLang rule that denies requests containing
// the target string with the given rule ID.
func SimpleBlockRule(id int, target string) string {
	return fmt.Sprintf(
		`SecRule ARGS|REQUEST_URI|REQUEST_HEADERS "@contains %s" "id:%d,phase:2,deny,status:403,msg:'%s blocked'"`,
		target, id, target,
	)
}

// toUnstructured converts a typed runtime.Object to an unstructured
// representation for use with the dynamic client. Panics on conversion
// failure since this indicates a programming error in the builder.
func toUnstructured(obj runtime.Object) *unstructured.Unstructured {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		panic(fmt.Sprintf("failed to convert %T to unstructured: %v", obj, err))
	}
	return &unstructured.Unstructured{Object: raw}
}

// -----------------------------------------------------------------------------
// Resource Builders
// -----------------------------------------------------------------------------

// BuildGateway builds an unstructured Gateway object with Istio annotations.
// If f.IstioGatewayRevision is non-empty, sets metadata.labels["istio.io/rev"] to
// that value (from ISTIO_GATEWAY_REVISION when using framework.New).
func (f *Framework) BuildGateway(namespace, name, gatewayClassName string) *unstructured.Unstructured {
	meta := map[string]any{
		"name":      name,
		"namespace": namespace,
		"annotations": map[string]any{
			"networking.istio.io/service-type": "ClusterIP",
		},
	}
	if f.IstioGatewayRevision != "" {
		meta["labels"] = map[string]any{
			"istio.io/rev": f.IstioGatewayRevision,
		}
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata":   meta,
			"spec": map[string]any{
				"gatewayClassName": gatewayClassName,
				"listeners": []any{
					map[string]any{
						"name":     "http",
						"port":     int64(80),
						"protocol": "HTTP",
						"allowedRoutes": map[string]any{
							"namespaces": map[string]any{
								"from": "All",
							},
						},
					},
				},
			},
		},
	}
}

// BuildRuleSet builds an unstructured RuleSet object.
// Each entry in configMapNames refers to a ConfigMap by name in the same
// namespace as the RuleSet.
func BuildRuleSet(namespace, name string, configMapNames []string) *unstructured.Unstructured {
	rules := make([]wafv1alpha1.RuleSourceReference, len(configMapNames))
	for i, n := range configMapNames {
		rules[i] = wafv1alpha1.RuleSourceReference{Name: n}
	}

	rs := &wafv1alpha1.RuleSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: wafv1alpha1.GroupVersion.String(),
			Kind:       "RuleSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: wafv1alpha1.RuleSetSpec{
			Rules: rules,
		},
	}
	return toUnstructured(rs)
}

// BuildEngine builds an unstructured Engine object.
func BuildEngine(namespace, name string, opts EngineOpts) *unstructured.Unstructured {
	if opts.WasmImage == "" {
		opts.WasmImage = defaultWasmImage()
	}
	if opts.FailurePolicy == "" {
		opts.FailurePolicy = wafv1alpha1.FailurePolicyFail
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5
	}

	var labelSelector *metav1.LabelSelector
	if len(opts.GatewayNames) > 0 {
		labelSelector = &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "gateway.networking.k8s.io/gateway-name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   opts.GatewayNames,
			}},
		}
	} else {
		workloadLabels := opts.WorkloadLabels
		if workloadLabels == nil && opts.GatewayName != "" {
			workloadLabels = map[string]string{
				"gateway.networking.k8s.io/gateway-name": opts.GatewayName,
			}
		}
		if workloadLabels == nil {
			workloadLabels = map[string]string{"app": "gateway"}
		}
		labelSelector = &metav1.LabelSelector{
			MatchLabels: workloadLabels,
		}
	}

	engine := &wafv1alpha1.Engine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: wafv1alpha1.GroupVersion.String(),
			Kind:       "Engine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: wafv1alpha1.EngineSpec{
			RuleSet: wafv1alpha1.RuleSetReference{
				Name: opts.RuleSetName,
			},
			FailurePolicy: opts.FailurePolicy,
			Driver: &wafv1alpha1.DriverConfig{
				Istio: &wafv1alpha1.IstioDriverConfig{
					Wasm: &wafv1alpha1.IstioWasmConfig{
						Image:            opts.WasmImage,
						Mode:             wafv1alpha1.IstioIntegrationModeGateway,
						WorkloadSelector: labelSelector,
						RuleSetCacheServer: &wafv1alpha1.RuleSetCacheServerConfig{
							PollIntervalSeconds: opts.PollInterval,
						},
					},
				},
			},
		},
	}
	return toUnstructured(engine)
}

// BuildHTTPRoute builds an unstructured HTTPRoute that routes all traffic
// from the named Gateway to the named backend Service on port 80.
func BuildHTTPRoute(namespace, name, gatewayName, backendName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"parentRefs": []any{
					map[string]any{
						"name": gatewayName,
					},
				},
				"rules": []any{
					map[string]any{
						"backendRefs": []any{
							map[string]any{
								"name": backendName,
								"port": int64(80),
							},
						},
					},
				},
			},
		},
	}
}

// -----------------------------------------------------------------------------
// Scenario - Resource Creation Methods
// -----------------------------------------------------------------------------

// CreateConfigMap creates a ConfigMap with WAF rules and registers cleanup.
func (s *Scenario) CreateConfigMap(namespace, name, rules string) {
	s.T.Helper()
	ctx := s.T.Context()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string]string{
			"rules": rules,
		},
	}
	_, err := s.F.KubeClient.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
	require.NoError(s.T, err, "create ConfigMap %s/%s", namespace, name)

	s.T.Logf("Created ConfigMap: %s/%s", namespace, name)
	s.OnCleanup(func() {
		// Background: test context may already be cancelled; cleanup must still run.
		if err := s.F.KubeClient.CoreV1().ConfigMaps(namespace).Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete ConfigMap %s/%s: %v", namespace, name, err)
		}
	})
}

// CreateGateway creates a Gateway resource and registers cleanup.
// The GatewayClass is determined by the GATEWAY_CLASS environment variable,
// defaulting to "istio" for Kind/standard Istio clusters.
// Set GATEWAY_CLASS=openshift-default for OpenShift environments.
func (s *Scenario) CreateGateway(namespace, name string) {
	s.T.Helper()
	gatewayClass := os.Getenv("GATEWAY_CLASS")
	if gatewayClass == "" {
		gatewayClass = "istio"
	}
	s.CreateGatewayWithClass(namespace, name, gatewayClass)
}

// CreateGatewayWithClass creates a Gateway resource using the provided GatewayClass and registers cleanup.
func (s *Scenario) CreateGatewayWithClass(namespace, name, gatewayClassName string) {
	s.T.Helper()

	// Serialize Gateway creation to avoid overwhelming Istio CA controller.
	// When many Gateways are created simultaneously, the CA controller races
	// to update istio-ca-root-cert ConfigMaps, causing conflicts and delays.
	gatewayMu.Lock()
	defer gatewayMu.Unlock()

	ctx := s.T.Context()

	// Create ServiceAccount before Gateway to prevent auth race condition.
	// Istio gateway pods use a ServiceAccount named "{gateway-name}-{class}".
	saName := fmt.Sprintf("%s-%s", name, gatewayClassName)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: namespace,
		},
	}
	_, err := s.F.KubeClient.CoreV1().ServiceAccounts(namespace).Create(ctx, sa, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			require.NoError(s.T, err, "create ServiceAccount %s/%s", namespace, saName)
		}
		s.T.Logf("ServiceAccount %s/%s already exists", namespace, saName)
	}

	obj := s.F.BuildGateway(namespace, name, gatewayClassName)
	_, err = s.F.DynamicClient.Resource(GatewayGVR).Namespace(namespace).Create(
		ctx, obj, metav1.CreateOptions{},
	)
	require.NoError(s.T, err, "create Gateway %s/%s", namespace, name)

	s.T.Logf("Created Gateway: %s/%s", namespace, name)
	s.OnCleanup(func() {
		// Background: test context may already be cancelled; cleanup must still run.
		if err := s.F.DynamicClient.Resource(GatewayGVR).Namespace(namespace).Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete Gateway %s/%s: %v", namespace, name, err)
		}
	})
}

// CreateRuleSet creates a RuleSet resource and registers cleanup. Fails the
// test on error. Use TryCreateRuleSet to get the error instead.
func (s *Scenario) CreateRuleSet(namespace, name string, configMapNames []string) {
	s.T.Helper()
	err := s.TryCreateRuleSet(namespace, name, configMapNames)
	require.NoError(s.T, err, "create RuleSet %s/%s", namespace, name)

	s.T.Logf("Created RuleSet: %s/%s", namespace, name)
	s.OnCleanup(func() {
		// Background: test context may already be cancelled; cleanup must still run.
		if err := s.F.DynamicClient.Resource(RuleSetGVR).Namespace(namespace).Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete RuleSet %s/%s: %v", namespace, name, err)
		}
	})
}

// TryCreateRuleSet attempts to create a RuleSet and returns any error.
// Use this when testing validation rejection.
func (s *Scenario) TryCreateRuleSet(namespace, name string, configMapNames []string) error {
	obj := BuildRuleSet(namespace, name, configMapNames)
	_, err := s.F.DynamicClient.Resource(RuleSetGVR).Namespace(namespace).Create(
		s.T.Context(), obj, metav1.CreateOptions{},
	)
	return err
}

// CreateEngine creates an Engine resource and registers cleanup. Fails the
// test on error. Use TryCreateEngine to get the error instead.
func (s *Scenario) CreateEngine(namespace, name string, opts EngineOpts) {
	s.T.Helper()
	err := s.TryCreateEngine(namespace, name, opts)
	require.NoError(s.T, err, "create Engine %s/%s", namespace, name)

	s.T.Logf("Created Engine: %s/%s", namespace, name)
	s.OnCleanup(func() {
		// Background: test context may already be cancelled; cleanup must still run.
		if err := s.F.DynamicClient.Resource(EngineGVR).Namespace(namespace).Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete Engine %s/%s: %v", namespace, name, err)
		}
	})
}

// TryCreateEngine attempts to create an Engine and returns any error.
// Use this when testing validation rejection.
func (s *Scenario) TryCreateEngine(namespace, name string, opts EngineOpts) error {
	obj := BuildEngine(namespace, name, opts)
	_, err := s.F.DynamicClient.Resource(EngineGVR).Namespace(namespace).Create(
		s.T.Context(), obj, metav1.CreateOptions{},
	)
	return err
}

// CreateHTTPRoute creates an HTTPRoute that routes traffic from the named
// Gateway to the named backend Service and registers cleanup.
func (s *Scenario) CreateHTTPRoute(namespace, name, gatewayName, backendName string) {
	s.T.Helper()
	ctx := s.T.Context()

	obj := BuildHTTPRoute(namespace, name, gatewayName, backendName)
	_, err := s.F.DynamicClient.Resource(HTTPRouteGVR).Namespace(namespace).Create(
		ctx, obj, metav1.CreateOptions{},
	)
	require.NoError(s.T, err, "create HTTPRoute %s/%s", namespace, name)

	s.T.Logf("Created HTTPRoute: %s/%s (gateway=%s, backend=%s)", namespace, name, gatewayName, backendName)
	s.OnCleanup(func() {
		if err := s.F.DynamicClient.Resource(HTTPRouteGVR).Namespace(namespace).Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete HTTPRoute %s/%s: %v", namespace, name, err)
		}
	})
}

// CreateEchoBackend deploys the Gateway API echo server (Deployment + Service)
// and waits for at least one pod to be Ready. The echo image defaults to
// ECHO_IMAGE env var or the built-in Gateway API conformance echo image.
func (s *Scenario) CreateEchoBackend(namespace, name string) {
	s.T.Helper()
	ctx := s.T.Context()

	echoImage := defaultEchoImage()
	replicas := int32(1)
	containerPort := int32(3000)
	servicePort := int32(80)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
					Annotations: map[string]string{
						"sidecar.istio.io/inject": "false",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  name,
						Image: echoImage,
						Ports: []corev1.ContainerPort{{
							ContainerPort: containerPort,
						}},
					}},
				},
			},
		},
	}
	_, err := s.F.KubeClient.AppsV1().Deployments(namespace).Create(ctx, dep, metav1.CreateOptions{})
	require.NoError(s.T, err, "create Deployment %s/%s", namespace, name)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{{
				Port:       servicePort,
				TargetPort: intstr.FromInt32(containerPort),
			}},
		},
	}
	_, err = s.F.KubeClient.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{})
	require.NoError(s.T, err, "create Service %s/%s", namespace, name)

	s.T.Logf("Created echo backend: %s/%s (image: %s)", namespace, name, echoImage)

	require.Eventually(s.T, func() bool {
		pods, listErr := s.F.KubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", name),
		})
		if listErr != nil || len(pods.Items) == 0 {
			return false
		}
		for _, pod := range pods.Items {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return true
				}
			}
		}
		return false
	}, DefaultTimeout, DefaultInterval, "echo backend %s/%s pod not ready", namespace, name)

	s.OnCleanup(func() {
		if err := s.F.KubeClient.AppsV1().Deployments(namespace).Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete Deployment %s/%s: %v", namespace, name, err)
		}
		if err := s.F.KubeClient.CoreV1().Services(namespace).Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete Service %s/%s: %v", namespace, name, err)
		}
	})
}

// -----------------------------------------------------------------------------
// Scenario - Resource Update Methods
// -----------------------------------------------------------------------------

// UpdateRuleSet replaces the spec.rules list of an existing RuleSet with the
// given ConfigMap names. Fails the test on error.
func (s *Scenario) UpdateRuleSet(namespace, name string, configMapNames []string) {
	s.T.Helper()
	ctx := s.T.Context()

	obj, err := s.F.DynamicClient.Resource(RuleSetGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(s.T, err, "get RuleSet %s/%s", namespace, name)

	rules := make([]any, len(configMapNames))
	for i, cm := range configMapNames {
		rules[i] = map[string]any{"name": cm}
	}
	err = unstructured.SetNestedSlice(obj.Object, rules, "spec", "rules")
	require.NoError(s.T, err, "set spec.rules on RuleSet %s/%s", namespace, name)

	_, err = s.F.DynamicClient.Resource(RuleSetGVR).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(s.T, err, "update RuleSet %s/%s", namespace, name)

	s.T.Logf("Updated RuleSet %s/%s with %v", namespace, name, configMapNames)
}

// AnnotateRuleSet adds or overwrites a single annotation on an existing RuleSet.
func (s *Scenario) AnnotateRuleSet(namespace, name, key, value string) {
	s.T.Helper()
	arg := fmt.Sprintf("%s=%s", key, value)
	out, err := s.F.Kubectl(namespace, "annotate", "ruleset", name, arg, "--overwrite").CombinedOutput()
	require.NoError(s.T, err, "annotate RuleSet %s/%s (%s): %s", namespace, name, arg, string(out))
}

// UpdateConfigMap replaces the rules data of an existing ConfigMap.
func (s *Scenario) UpdateConfigMap(namespace, name, rules string) {
	s.T.Helper()
	ctx := s.T.Context()

	cm, err := s.F.KubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(s.T, err, "get ConfigMap %s/%s", namespace, name)

	cm.Data = map[string]string{"rules": rules}
	_, err = s.F.KubeClient.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(s.T, err, "update ConfigMap %s/%s", namespace, name)

	s.T.Logf("Updated ConfigMap %s/%s", namespace, name)
}
