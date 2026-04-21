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
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestCacheServerAuthentication validates that the cache server properly
// authenticates requests using Kubernetes ServiceAccount JWT tokens.
func TestCacheServerAuthentication(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("cache-auth")

	s.Step("create gateway")
	s.CreateGateway(ns, "auth-gateway")
	s.ExpectGatewayProgrammed(ns, "auth-gateway")

	s.Step("deploy rules and create ruleset")
	s.CreateRuleSource(ns, "test-rules", `SecRuleEngine On`)
	s.CreateRuleSet(ns, "auth-test", []string{"test-rules"}, nil)
	s.ExpectRuleSetReady(ns, "auth-test")

	s.Step("create engine")
	s.CreateEngine(ns, "auth-test", framework.EngineOpts{
		RuleSetName: "auth-test",
		GatewayName: "auth-gateway",
	})

	s.Step("verify cache client ServiceAccount was created")
	saName := s.ExpectCacheClientSAExists(ns, "auth-test")

	s.Step("port-forward to operator cache server")
	cs := s.ProxyToCacheServer(operatorNamespace)

	s.Step("verify that wasmplugin was created with a valid token")
	wasmplugin := s.ExpectWasmPluginExists(ns, "coraza-engine-auth-test")
	validToken, found, err := unstructured.NestedString(wasmplugin.Object, "spec", "pluginConfig", "cache_token")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, validToken)

	s.Step("create valid SA token with correct audience")
	cacheKey := fmt.Sprintf("%s/auth-test", ns)

	s.Step("test: valid token returns cached rules")
	result := cs.GetWithBearer("/rules/"+cacheKey, validToken)
	require.NoError(t, result.Err)
	require.Equal(t, http.StatusOK, result.StatusCode,
		"valid token should return 200, got %d: %s", result.StatusCode, string(result.Body))

	var entry cache.RuleSetEntry
	require.NoError(t, json.Unmarshal(result.Body, &entry))
	assert.NotEmpty(t, entry.UUID)
	assert.Contains(t, entry.Rules, "SecRuleEngine On")

	s.Step("test: valid token works for /latest endpoint")
	result = cs.GetWithBearer("/rules/"+cacheKey+"/latest", validToken)
	require.NoError(t, result.Err)
	require.Equal(t, http.StatusOK, result.StatusCode)
	var latest cache.LatestResponse
	require.NoError(t, json.Unmarshal(result.Body, &latest))
	assert.Equal(t, entry.UUID, latest.UUID)

	s.Step("test: missing token returns 403")
	result = cs.GetWithBearer("/rules/"+cacheKey, "")
	require.NoError(t, result.Err)
	assert.Equal(t, http.StatusForbidden, result.StatusCode)

	s.Step("test: invalid/garbage token returns 403")
	result = cs.GetWithBearer("/rules/"+cacheKey, "garbage-token-value")
	require.NoError(t, result.Err)
	assert.Equal(t, http.StatusForbidden, result.StatusCode)

	s.Step("test: valid token for wrong ruleset returns 403")
	s.CreateRuleSource(ns, "other-rules", `SecRuleEngine Off`)
	s.CreateRuleSet(ns, "other-test", []string{"other-rules"}, nil)
	s.ExpectRuleSetReady(ns, "other-test")

	s.CreateEngine(ns, "other-test", framework.EngineOpts{
		RuleSetName: "other-test",
		GatewayName: "auth-gateway",
	})

	otherSAName := s.ExpectCacheClientSAExists(ns, "other-test")

	otherCacheKey := fmt.Sprintf("%s/other-test", ns)
	wrongToken := s.CreateCacheServerToken(ns, otherSAName, cache.Audience(otherCacheKey))
	result = cs.GetWithBearer("/rules/"+cacheKey, wrongToken)
	require.NoError(t, result.Err)
	assert.Equal(t, http.StatusForbidden, result.StatusCode,
		"token for different ruleset should return 403")

	s.Step("test: token with wrong audience returns 403")
	wrongAudienceToken := s.CreateCacheServerToken(ns, saName, "wrong-audience")
	result = cs.GetWithBearer("/rules/"+cacheKey, wrongAudienceToken)
	require.NoError(t, result.Err)
	assert.Equal(t, http.StatusForbidden, result.StatusCode,
		"token with wrong audience should return 403")
}
