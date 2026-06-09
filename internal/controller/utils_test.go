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

func TestBuildCacheReadyMessage(t *testing.T) {
	t.Run("without unsupported message", func(t *testing.T) {
		msg := buildCacheReadyMessage("ns", "my-rules", "")
		assert.Equal(t, "Successfully cached rules for ns/my-rules", msg)
	})

	t.Run("with unsupported message", func(t *testing.T) {
		msg := buildCacheReadyMessage("ns", "my-rules", "found unsupported rule 950150")
		assert.Contains(t, msg, "Successfully cached rules for ns/my-rules")
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
