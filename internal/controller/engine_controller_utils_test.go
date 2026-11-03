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
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

func TestEngineMatchesLabels(t *testing.T) {
	podLabels := map[string]string{
		"app":                                    "gateway",
		"gateway.networking.k8s.io/gateway-name": "my-gw",
	}

	t.Run("empty targetRef returns false", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{},
		}
		assert.False(t, engineMatchesLabels(engine, podLabels))
	})

	t.Run("empty target name returns false", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "",
			}},
		}
		assert.False(t, engineMatchesLabels(engine, podLabels))
	})

	t.Run("matching gateway name returns true", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "my-gw",
			}},
		}
		assert.True(t, engineMatchesLabels(engine, podLabels))
	})

	t.Run("non-matching gateway name returns false", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "other-gw",
			}},
		}
		assert.False(t, engineMatchesLabels(engine, podLabels))
	})

	t.Run("nil pod labels returns false", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "my-gw",
			}},
		}
		assert.False(t, engineMatchesLabels(engine, nil))
	})

	t.Run("pod without gateway label returns false", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "my-gw",
			}},
		}
		assert.False(t, engineMatchesLabels(engine, map[string]string{"app": "gateway"}))
	})
}

// TestTargetLabelSelector_SecurityRegressions verifies the runtime guard in
// targetLabelSelector rejects names that would produce unmatchable label
// selectors (silent WAF bypass).
func TestTargetLabelSelector_SecurityRegressions(t *testing.T) {
	t.Run("name exceeding 63 chars returns nil selector", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "a234567890123456789012345678901234567890123456789012345678901234",
			}},
		}
		assert.Nil(t, targetLabelSelector(engine),
			"names longer than 63 chars must be rejected to prevent silent WAF bypass")
	})

	t.Run("name with spaces returns nil selector", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "my gateway",
			}},
		}
		assert.Nil(t, targetLabelSelector(engine),
			"names with spaces are invalid label values and must be rejected")
	})

	t.Run("name starting with hyphen returns nil selector", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "-invalid-start",
			}},
		}
		assert.Nil(t, targetLabelSelector(engine),
			"names starting with hyphen are invalid label values")
	})

	t.Run("valid label value returns non-nil selector", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "my-gw-123",
			}},
		}
		sel := targetLabelSelector(engine)
		assert.NotNil(t, sel)
		assert.Equal(t, "my-gw-123", sel.MatchLabels["gateway.networking.k8s.io/gateway-name"])
	})

	t.Run("single letter name is valid", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "a",
			}},
		}
		sel := targetLabelSelector(engine)
		assert.NotNil(t, sel)
		assert.Equal(t, "a", sel.MatchLabels["gateway.networking.k8s.io/gateway-name"])
	})

	t.Run("name starting with digit returns nil selector", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "123-gw",
			}},
		}
		assert.Nil(t, targetLabelSelector(engine),
			"DNS-1035 labels must start with a letter")
	})

	t.Run("dotted name returns nil selector", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "my.gateway",
			}},
		}
		assert.Nil(t, targetLabelSelector(engine),
			"dots are not allowed in DNS-1035 labels")
	})

	t.Run("uppercase name returns nil selector", func(t *testing.T) {
		engine := &wafv1alpha1.Engine{
			Spec: wafv1alpha1.EngineSpec{Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "My-Gateway",
			}},
		}
		assert.Nil(t, targetLabelSelector(engine),
			"uppercase is not allowed in DNS-1035 labels")
	})

	t.Run("nil engine returns nil selector", func(t *testing.T) {
		assert.Nil(t, targetLabelSelector(nil))
	})
}

func TestBuildAcceptReadyMessage(t *testing.T) {
	t.Run("without unsupported message", func(t *testing.T) {
		msg := buildAcceptReadyMessage("ns", "my-rules", "")
		assert.Equal(t, "Successfully accepted rules for ns/my-rules", msg)
	})

	t.Run("with unsupported message", func(t *testing.T) {
		msg := buildAcceptReadyMessage("ns", "my-rules", "found unsupported rule 950150")
		assert.Contains(t, msg, "Successfully accepted rules for ns/my-rules")
		assert.Contains(t, msg, "[annotation override]")
		assert.Contains(t, msg, "950150")
	})
}

func TestCollectRequests(t *testing.T) {
	engines := []wafv1alpha1.Engine{
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns2"}},
	}

	t.Run("empty slice returns nil", func(t *testing.T) {
		got := collectRequests([]wafv1alpha1.Engine{}, func(e *wafv1alpha1.Engine) bool {
			return true
		})
		assert.Nil(t, got)
	})

	t.Run("no matches returns nil", func(t *testing.T) {
		got := collectRequests(engines, func(e *wafv1alpha1.Engine) bool {
			return false
		})
		assert.Nil(t, got)
	})

	t.Run("all match", func(t *testing.T) {
		got := collectRequests(engines, func(e *wafv1alpha1.Engine) bool {
			return true
		})
		assert.Equal(t, []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: "a", Namespace: "ns1"}},
			{NamespacedName: types.NamespacedName{Name: "b", Namespace: "ns1"}},
			{NamespacedName: types.NamespacedName{Name: "c", Namespace: "ns2"}},
		}, got)
	})

	t.Run("partial match", func(t *testing.T) {
		got := collectRequests(engines, func(e *wafv1alpha1.Engine) bool {
			return e.Namespace == "ns1"
		})
		assert.Equal(t, []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: "a", Namespace: "ns1"}},
			{NamespacedName: types.NamespacedName{Name: "b", Namespace: "ns1"}},
		}, got)
	})
}
