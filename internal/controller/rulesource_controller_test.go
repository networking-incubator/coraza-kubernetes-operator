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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

func TestRuleSourceReconciler_Validated(t *testing.T) {
	ctx := context.Background()
	rs := utils.NewTestRuleSource("rs-ctrl-valid", testNamespace,
		`SecRule REQUEST_URI "@contains /x" "id:1,phase:1,pass,nolog"`)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	rec := &RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder()}
	_, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}, rs))
	ready := apimeta.FindStatusCondition(rs.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, ruleSourceReadyReasonValidated, ready.Reason)
}

func TestRuleSourceReconciler_InvalidRules(t *testing.T) {
	ctx := context.Background()
	rs := utils.NewTestRuleSource("rs-ctrl-bad", testNamespace, `SecDefaultActionXPTO "INVALID"`)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	rec := &RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder()}
	_, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}, rs))
	deg := apimeta.FindStatusCondition(rs.Status.Conditions, conditionDegraded)
	require.NotNil(t, deg)
	assert.Equal(t, metav1.ConditionTrue, deg.Status)
	assert.Equal(t, ruleSourceDegradedReasonInvalidRules, deg.Reason)
}

func TestRuleSourceReconciler_ValidationSkipped(t *testing.T) {
	ctx := context.Background()
	rs := utils.NewTestRuleSource("rs-ctrl-skip", testNamespace, "SecCollectionTimeout 1")
	rs.Annotations = map[string]string{wafv1alpha1.AnnotationSkipValidation: "false"}
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	rec := &RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder()}
	_, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}, rs))
	ready := apimeta.FindStatusCondition(rs.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, ruleSourceReadyReasonValidationSkipped, ready.Reason)
}
