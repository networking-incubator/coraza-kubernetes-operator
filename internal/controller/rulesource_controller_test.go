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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

	reg := prometheus.NewRegistry()
	m, err := NewCorazaMetrics(reg)
	require.NoError(t, err)

	rec := &RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder(), Metrics: m}
	_, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}, rs))
	deg := apimeta.FindStatusCondition(rs.Status.Conditions, conditionDegraded)
	require.NotNil(t, deg)
	assert.Equal(t, metav1.ConditionTrue, deg.Status)
	assert.Equal(t, ruleSourceDegradedReasonInvalidRules, deg.Reason)
	assert.Equal(t, float64(1), gatherValidationCounter(t, reg, "invalid"))
}

func TestRuleSourceReconciler_AuditLogTargets(t *testing.T) {
	ctx := context.Background()

	valid := utils.NewTestRuleSource("rs-ctrl-audit-stream", testNamespace,
		"SecRuleEngine On\nSecAuditEngine On\nSecAuditLogType Serial\nSecAuditLogFormat JSON")
	require.NoError(t, k8sClient.Create(ctx, valid))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, valid) })

	rec := &RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder()}
	_, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: valid.Name, Namespace: valid.Namespace}})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: valid.Name, Namespace: valid.Namespace}, valid))
	ready := apimeta.FindStatusCondition(valid.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, ruleSourceReadyReasonValidated, ready.Reason)

	invalid := utils.NewTestRuleSource("rs-ctrl-audit-file", testNamespace,
		"SecRuleEngine On\nSecAuditEngine On\nSecAuditLog /var/log/audit.log")
	require.NoError(t, k8sClient.Create(ctx, invalid))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, invalid) })

	_, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: invalid.Name, Namespace: invalid.Namespace}})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: invalid.Name, Namespace: invalid.Namespace}, invalid))
	deg := apimeta.FindStatusCondition(invalid.Status.Conditions, conditionDegraded)
	require.NotNil(t, deg)
	assert.Equal(t, ruleSourceDegradedReasonInvalidRules, deg.Reason)
	assert.Contains(t, deg.Message, "SecAuditLog is not supported by envoy-coraza")
}

func TestRuleSourceReconciler_PatchOnlyFragment(t *testing.T) {
	ctx := context.Background()
	rs := utils.NewTestRuleSource("rs-ctrl-patch-only", testNamespace,
		`SecRuleUpdateTargetById 932240 "!REQUEST_COOKIES:/^_ga(?:_\w+)?$/"`)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	reg := prometheus.NewRegistry()
	m, err := NewCorazaMetrics(reg)
	require.NoError(t, err)

	rec := &RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder(), Metrics: m}
	_, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}, rs))
	ready := apimeta.FindStatusCondition(rs.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, ruleSourceReadyReasonValidated, ready.Reason)

	deg := apimeta.FindStatusCondition(rs.Status.Conditions, conditionDegraded)
	assert.Nil(t, deg)
	assert.Equal(t, float64(1), gatherValidationCounter(t, reg, "skipped"))
	assert.Equal(t, float64(0), gatherValidationCounter(t, reg, "valid"))
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_rulesource_validation_duration_seconds"),
		"skipped outcomes must not emit validation duration histograms")
}

func TestRuleSourceReconciler_MalformedPatchOnlyFragment(t *testing.T) {
	// Patch-only classification is syntactic; Coraza cannot validate patches in isolation.
	// Malformed patch text still reaches Ready here; RuleSet aggregate validation is authoritative.
	ctx := context.Background()
	rs := utils.NewTestRuleSource("rs-ctrl-patch-bad", testNamespace,
		`SecRuleUpdateTargetById 999999 "this is not valid patch syntax"`)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	reg := prometheus.NewRegistry()
	m, err := NewCorazaMetrics(reg)
	require.NoError(t, err)

	rec := &RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder(), Metrics: m}
	_, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}, rs))
	ready := apimeta.FindStatusCondition(rs.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, ruleSourceReadyReasonValidated, ready.Reason)
	assert.Equal(t, float64(1), gatherValidationCounter(t, reg, "skipped"))
	assert.Equal(t, float64(0), gatherValidationCounter(t, reg, "valid"))
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_rulesource_validation_duration_seconds"),
		"skipped outcomes must not emit validation duration histograms")
}

// TestRuleSourceReconciler_PatchOnlyFragmentWithBadInclude covers the guard for
// patch-only fragments: Coraza is skipped, but an Include the plugin cannot
// resolve must still be rejected at source level.
func TestRuleSourceReconciler_PatchOnlyFragmentWithBadInclude(t *testing.T) {
	ctx := context.Background()
	rs := utils.NewTestRuleSource("rs-ctrl-patch-bad-include", testNamespace,
		"SecRuleRemoveById 1\nInclude @notanalias")
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	reg := prometheus.NewRegistry()
	m, err := NewCorazaMetrics(reg)
	require.NoError(t, err)

	rec := &RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder(), Metrics: m}
	_, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}})
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}, rs))
	deg := apimeta.FindStatusCondition(rs.Status.Conditions, conditionDegraded)
	require.NotNil(t, deg)
	assert.Equal(t, metav1.ConditionTrue, deg.Status)
	assert.Equal(t, ruleSourceDegradedReasonInvalidRules, deg.Reason)
	assert.Contains(t, deg.Message, "crs/crs-setup.conf")
	assert.Equal(t, float64(1), gatherValidationCounter(t, reg, "invalid"))
	assert.Equal(t, float64(0), gatherValidationCounter(t, reg, "skipped"))
}

func TestRuleSourceReconciler_MetricsNotIncrementedOnResync(t *testing.T) {
	ctx := context.Background()
	rs := utils.NewTestRuleSource("rs-ctrl-resync", testNamespace,
		`SecRule REQUEST_URI "@contains /x" "id:1,phase:1,pass,nolog"`)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	reg := prometheus.NewRegistry()
	m, err := NewCorazaMetrics(reg)
	require.NoError(t, err)

	rec := &RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder(), Metrics: m}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}}

	_, err = rec.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, float64(1), gatherValidationCounter(t, reg, "valid"))

	_, err = rec.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, float64(1), gatherValidationCounter(t, reg, "valid"),
		"second reconcile with unchanged Ready condition must not increment validation counter")
}

// TestRuleSourceReconciler_MetricsRecordOnSuccess verifies that after a
// successful reconcile the RecordRuleSource path fires and coraza_rulesource_info
// is set to 1 for the resource.
func TestRuleSourceReconciler_MetricsRecordOnSuccess(t *testing.T) {
	ctx := context.Background()
	rs := utils.NewTestRuleSource("rs-metrics-ok", testNamespace,
		`SecRule REQUEST_URI "@contains /test" "id:10,phase:1,pass,nolog"`)
	require.NoError(t, k8sClient.Create(ctx, rs))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, rs) })

	reg := prometheus.NewRegistry()
	m, err := NewCorazaMetrics(reg)
	require.NoError(t, err)

	rec := &RuleSourceReconciler{
		Client:   k8sClient,
		Recorder: utils.NewTestRecorder(),
		Metrics:  m,
	}
	_, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: rs.Name, Namespace: rs.Namespace}})
	require.NoError(t, err)

	// The defer in Reconcile fires RecordRuleSource: info gauge must be 1.
	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_rulesource_info"),
		"coraza_rulesource_info must be emitted after successful reconcile")
	assert.Equal(t, float64(1), gatherValidationCounter(t, reg, "valid"))
}

// TestRuleSourceReconciler_MetricsForgetOnNotFound verifies that reconciling a
// deleted RuleSource calls ForgetRuleSource and the info series is cleared.
func TestRuleSourceReconciler_MetricsForgetOnNotFound(t *testing.T) {
	ctx := context.Background()

	reg := prometheus.NewRegistry()
	m, err := NewCorazaMetrics(reg)
	require.NoError(t, err)

	// Pre-populate the series to simulate a previously-recorded RuleSource.
	m.RecordRuleSource(minimalRuleSource(testNamespace, "rs-metrics-gone"))
	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_rulesource_info"),
		"pre-condition: info series must exist before reconcile")

	rec := &RuleSourceReconciler{
		Client:   k8sClient,
		Recorder: utils.NewTestRecorder(),
		Metrics:  m,
	}
	// Reconcile a resource that does not exist in the cluster.
	_, err = rec.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "rs-metrics-gone", Namespace: testNamespace},
	})
	require.NoError(t, err)

	// ForgetRuleSource must have deleted the series.
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_rulesource_info"),
		"coraza_rulesource_info must be gone after not-found reconcile")
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_rulesource_condition"),
		"coraza_rulesource_condition must be gone after not-found reconcile")
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
