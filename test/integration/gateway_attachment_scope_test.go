//go:build integration

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

package integration

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestRootNamespaceEnvoyFilterTargetRefs scopes an Engine in Istio's config
// root namespace to its Gateway, despite a same-named Gateway in another
// namespace.
func TestRootNamespaceEnvoyFilterTargetRefs(t *testing.T) {
	s := fw.NewScenario(t)
	foreignNS := s.GenerateNamespace("gateway-scope")
	const rootNamespace = "coraza-system"
	gatewayName := foreignNS
	gatewayClass := os.Getenv("GATEWAY_CLASS")
	if gatewayClass == "" {
		gatewayClass = "istio"
	}

	// Scenario.CreateGateway creates this ServiceAccount but only cleans up the
	// Gateway. The root namespace is shared, so register cleanup only when this
	// test is creating the uniquely named account.
	rootServiceAccount := gatewayName + "-" + gatewayClass
	_, err := s.F.KubeClient.CoreV1().ServiceAccounts(rootNamespace).Get(
		t.Context(), rootServiceAccount, metav1.GetOptions{},
	)
	switch {
	case apierrors.IsNotFound(err):
		s.OnCleanup(func() {
			err := s.F.KubeClient.CoreV1().ServiceAccounts(rootNamespace).Delete(
				context.Background(), rootServiceAccount, metav1.DeleteOptions{},
			)
			if err != nil && !apierrors.IsNotFound(err) {
				t.Logf("cleanup: failed to delete ServiceAccount %s/%s: %v", rootNamespace, rootServiceAccount, err)
			}
		})
	case err != nil:
		require.NoError(t, err)
	default:
		t.Fatalf("ServiceAccount %s/%s already exists", rootNamespace, rootServiceAccount)
	}

	s.Step("create same-named Gateways in the root and foreign namespaces")
	s.CreateGateway(rootNamespace, gatewayName)
	s.ExpectGatewayProgrammed(rootNamespace, gatewayName)
	s.CreateGateway(foreignNS, gatewayName)
	s.ExpectGatewayProgrammed(foreignNS, gatewayName)

	ruleSourceName := "scope-rules-" + gatewayName
	ruleSetName := "scope-ruleset-" + gatewayName
	engineName := "scope-engine-" + gatewayName
	s.Step("create a fail-closed Engine targeting the root-namespace Gateway")
	s.CreateRuleSource(rootNamespace, ruleSourceName, "SecRuleEngine On\n"+framework.SimpleBlockRule(19001, "blocked"))
	s.CreateRuleSet(rootNamespace, ruleSetName, []string{ruleSourceName}, nil)
	s.CreateEngine(rootNamespace, engineName, framework.EngineOpts{
		RuleSetName: ruleSetName,
		GatewayName: gatewayName,
	})
	s.ExpectEngineReady(rootNamespace, engineName)

	envoyFilter := s.ExpectEnvoyFilterExists(rootNamespace, "coraza-engine-"+engineName)
	targetRefs, found, err := unstructured.NestedSlice(envoyFilter.Object, "spec", "targetRefs")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []any{
		map[string]any{
			"group": "gateway.networking.k8s.io",
			"kind":  "Gateway",
			"name":  gatewayName,
		},
	}, targetRefs)

	s.Step("deploy independent backends and routes")
	rootBackend := "root-" + gatewayName
	foreignBackend := "foreign-" + gatewayName
	s.CreateEchoBackend(rootNamespace, rootBackend)
	s.CreateEchoBackend(foreignNS, foreignBackend)
	s.CreateHTTPRoute(rootNamespace, "root-route-"+gatewayName, gatewayName, rootBackend)
	s.CreateHTTPRoute(foreignNS, "foreign-route-"+gatewayName, gatewayName, foreignBackend)

	rootGateway := s.ProxyToGateway(rootNamespace, gatewayName)
	foreignGateway := s.ProxyToGateway(foreignNS, gatewayName)

	s.Step("verify the intended Gateway enforces the WAF and the foreign Gateway does not")
	rootGateway.ExpectBlocked("/?test=blocked")
	rootGateway.ExpectAllowed("/?test=safe")
	foreignGateway.ExpectAllowed("/?test=blocked")
}
