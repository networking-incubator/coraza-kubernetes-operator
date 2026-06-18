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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Constants
// -----------------------------------------------------------------------------

const (
	labelCondition         = "condition"
	metricRuleSetSources   = "coraza_ruleset_sources"
	metricRuleSetDataFiles = "coraza_ruleset_data_files"
)

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func newTestRegistry() *prometheus.Registry {
	return prometheus.NewPedanticRegistry()
}

func newTestMetrics(t *testing.T) (*CorazaMetrics, *prometheus.Registry) {
	t.Helper()
	reg := newTestRegistry()
	m, err := NewCorazaMetrics(reg)
	require.NoError(t, err)
	return m, reg
}

// minimalEngine returns an Engine with the minimum valid fields set.
func minimalEngine(ns, name, gatewayName string) *wafv1alpha1.Engine {
	return &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: wafv1alpha1.EngineSpec{
			Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: gatewayName,
			},
			RuleSet: wafv1alpha1.RuleSetReference{
				Name: "rs",
			},
			FailurePolicy: wafv1alpha1.FailurePolicyFail,
		},
	}
}

// minimalRuleSet returns a RuleSet with the minimum valid fields set.
func minimalRuleSet(ns, name string, sources int, data int) *wafv1alpha1.RuleSet {
	rs := &wafv1alpha1.RuleSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: wafv1alpha1.RuleSetSpec{},
	}
	for i := 0; i < sources; i++ {
		rs.Spec.Sources = append(rs.Spec.Sources, wafv1alpha1.SourceReference{Name: "src"})
	}
	for i := 0; i < data; i++ {
		rs.Spec.Data = append(rs.Spec.Data, wafv1alpha1.DataReference{Name: "dat"})
	}
	return rs
}

// minimalRuleSource returns a RuleSource with the minimum valid fields set.
func minimalRuleSource(ns, name string) *wafv1alpha1.RuleSource {
	return &wafv1alpha1.RuleSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: wafv1alpha1.RuleSourceSpec{
			Rules: "SecRule REQUEST_URI \"@streq /admin\" \"id:1,phase:1,deny\"",
		},
	}
}

// minimalRuleData returns a RuleData with the minimum valid fields set.
func minimalRuleData(ns, name string) *wafv1alpha1.RuleData {
	return &wafv1alpha1.RuleData{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: wafv1alpha1.RuleDataSpec{
			Files: map[string]string{"words.txt": "foo\nbar\n"},
		},
	}
}

// -----------------------------------------------------------------------------
// Tests — NewCorazaMetrics
// -----------------------------------------------------------------------------

// TestNewCorazaMetricsRegistration verifies that NewCorazaMetrics succeeds on a
// fresh registry and returns an error when called a second time (all 14
// GaugeVecs conflict).
func TestNewCorazaMetricsRegistration(t *testing.T) {
	reg := newTestRegistry()

	m, err := NewCorazaMetrics(reg)
	require.NoError(t, err)
	assert.NotNil(t, m)

	_, err = NewCorazaMetrics(reg)
	require.Error(t, err, "second NewCorazaMetrics with the same registry must fail")
}

// TestNewCorazaMetricsGatherAndLint registers all 14 metrics, records one of
// each resource type, and verifies that GatherAndLint finds no problems.
func TestNewCorazaMetricsGatherAndLint(t *testing.T) {
	m, reg := newTestMetrics(t)

	m.RecordEngine(minimalEngine("default", "my-engine", "gw"))
	m.RecordRuleSet(minimalRuleSet("default", "my-rs", 1, 0))
	m.RecordRuleSource(minimalRuleSource("default", "my-src"))
	m.RecordRuleData(minimalRuleData("default", "my-rd"))
	m.SetEnginesTotal("default", 1)
	m.SetRuleSetsTotal("default", 1)
	m.SetRuleSourcesTotal("default", 1)
	m.SetRuleDatasTotal("default", 1)

	problems, err := testutil.GatherAndLint(reg,
		"coraza_engine_info", "coraza_engine_condition", "coraza_engines",
		"coraza_ruleset_info", "coraza_ruleset_condition", "coraza_rulesets",
		metricRuleSetSources, metricRuleSetDataFiles,
		"coraza_rulesource_info", "coraza_rulesource_condition", "coraza_rulesources",
		"coraza_ruledata_info", "coraza_ruledata_condition", "coraza_ruledatas",
	)
	require.NoError(t, err)
	assert.Empty(t, problems, "lint problems: %v", problems)
}

// -----------------------------------------------------------------------------
// Tests — Engine
// -----------------------------------------------------------------------------

// TestCorazaMetricsRecordEngine verifies that RecordEngine emits info and
// condition gauges with the expected label values and numeric encodings.
func TestCorazaMetricsRecordEngine(t *testing.T) {
	engine := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "ns"},
		Spec: wafv1alpha1.EngineSpec{
			Target:        wafv1alpha1.EngineTarget{Type: wafv1alpha1.EngineTargetTypeGateway, Name: "my-gw"},
			RuleSet:       wafv1alpha1.RuleSetReference{Name: "my-rs"},
			FailurePolicy: wafv1alpha1.FailurePolicyAllow,
			Driver:        wafv1alpha1.DriverConfig{Type: wafv1alpha1.DriverTypeWasm},
		},
		Status: &wafv1alpha1.EngineStatus{
			Conditions: []metav1.Condition{
				{Type: conditionReady, Status: metav1.ConditionTrue, Reason: "r", LastTransitionTime: metav1.Now()},
			},
		},
	}

	m, reg := newTestMetrics(t)
	m.RecordEngine(engine)

	// info gauge
	count := testutil.CollectAndCount(reg, "coraza_engine_info")
	assert.Equal(t, 1, count)

	gathered, err := reg.Gather()
	require.NoError(t, err)

	infoLabels := make(map[string]string)
	condValues := make(map[string]float64)
	for _, mf := range gathered {
		switch mf.GetName() {
		case "coraza_engine_info":
			for _, lp := range mf.GetMetric()[0].GetLabel() {
				infoLabels[lp.GetName()] = lp.GetValue()
			}
		case "coraza_engine_condition":
			for _, m := range mf.GetMetric() {
				var ct string
				for _, lp := range m.GetLabel() {
					if lp.GetName() == labelCondition {
						ct = lp.GetValue()
					}
				}
				condValues[ct] = m.GetGauge().GetValue()
			}
		}
	}

	assert.Equal(t, "my-gw", infoLabels["target_name"])
	assert.Equal(t, "Gateway", infoLabels["target_type"])
	assert.Equal(t, "wasm", infoLabels["driver_type"])
	assert.Equal(t, "my-rs", infoLabels["ruleset_name"])
	assert.Equal(t, "allow", infoLabels["failure_policy"])
	assert.Equal(t, float64(1), condValues[conditionReady])
	assert.Equal(t, float64(-1), condValues[conditionProgressing])
	assert.Equal(t, float64(-1), condValues[conditionDegraded])
	// engineConditionTypes includes conditionAccepted — must be emitted as -1 (unknown).
	assert.Equal(t, float64(-1), condValues[conditionAccepted])
}

// TestCorazaMetricsForgetEngine verifies that ForgetEngine removes all series
// for the named engine from the registry.
func TestCorazaMetricsForgetEngine(t *testing.T) {
	engine := minimalEngine("default", "eng", "gw")
	m, reg := newTestMetrics(t)
	m.RecordEngine(engine)

	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_engine_info"))

	m.ForgetEngine("default", "eng")

	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_engine_info"))
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_engine_condition"))
}

// TestCorazaMetricsEngineSpecChangeClearsStale verifies that RecordEngine with
// new spec label values (e.g. different target) removes old info series and
// creates a new one — no stale series remain.
func TestCorazaMetricsEngineSpecChangeClearsStale(t *testing.T) {
	engine := minimalEngine("default", "eng", "gw-old")
	m, reg := newTestMetrics(t)
	m.RecordEngine(engine)

	// Simulate spec change: new target name.
	engine.Spec.Target.Name = "gw-new"
	m.RecordEngine(engine)

	// Only one info series must exist — the old one must be gone.
	count := testutil.CollectAndCount(reg, "coraza_engine_info")
	assert.Equal(t, 1, count, "spec change must not leave stale info series")

	gathered, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range gathered {
		if mf.GetName() != "coraza_engine_info" {
			continue
		}
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			if lp.GetName() == "target_name" {
				assert.Equal(t, "gw-new", lp.GetValue(), "info series should have new target label")
			}
		}
	}
}

// TestCorazaMetricsEngineNilStatus verifies that an Engine with nil Status
// does not panic and emits -1.0 for all conditions.
func TestCorazaMetricsEngineNilStatus(t *testing.T) {
	engine := minimalEngine("default", "nil-engine", "gw")
	assert.Nil(t, engine.Status)

	m, reg := newTestMetrics(t)
	m.RecordEngine(engine)

	gathered, err := reg.Gather()
	require.NoError(t, err)

	for _, mf := range gathered {
		if mf.GetName() != "coraza_engine_condition" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			assert.Equal(t, float64(-1), metric.GetGauge().GetValue(),
				"nil Status should produce -1 for all conditions")
		}
	}
}

// TestCorazaMetricsSetEnginesTotal verifies per-namespace Engine count gauges.
func TestCorazaMetricsSetEnginesTotal(t *testing.T) {
	m, reg := newTestMetrics(t)
	m.SetEnginesTotal("ns-a", 3)
	m.SetEnginesTotal("ns-b", 1)

	gathered, err := reg.Gather()
	require.NoError(t, err)

	totals := make(map[string]float64)
	for _, mf := range gathered {
		if mf.GetName() != "coraza_engines" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			var ns string
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "namespace" {
					ns = lp.GetValue()
				}
			}
			totals[ns] = metric.GetGauge().GetValue()
		}
	}

	assert.Equal(t, float64(3), totals["ns-a"])
	assert.Equal(t, float64(1), totals["ns-b"])
}

// -----------------------------------------------------------------------------
// Tests — RuleSource
// -----------------------------------------------------------------------------

// TestCorazaMetricsRecordRuleSource verifies condition encoding for RuleSource.
func TestCorazaMetricsRecordRuleSource(t *testing.T) {
	rs := minimalRuleSource("default", "my-rs")
	rs.Status = wafv1alpha1.RuleSourceStatus{
		Conditions: []metav1.Condition{
			{
				Type:               conditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             ruleSourceReadyReasonValidated,
				LastTransitionTime: metav1.Now(),
			},
			{
				Type:               conditionDegraded,
				Status:             metav1.ConditionFalse,
				Reason:             ruleSourceDegradedReasonInvalidRules,
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	m, reg := newTestMetrics(t)
	m.RecordRuleSource(rs)

	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_rulesource_info"))

	gathered, err := reg.Gather()
	require.NoError(t, err)

	condValues := make(map[string]float64)
	for _, mf := range gathered {
		if mf.GetName() != "coraza_rulesource_condition" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			var ct string
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == labelCondition {
					ct = lp.GetValue()
				}
			}
			condValues[ct] = metric.GetGauge().GetValue()
		}
	}

	assert.Equal(t, float64(1), condValues[conditionReady])
	assert.Equal(t, float64(0), condValues[conditionDegraded])
}

// TestCorazaMetricsForgetRuleSource verifies that ForgetRuleSource removes all
// series for the named rulesource.
func TestCorazaMetricsForgetRuleSource(t *testing.T) {
	rs := minimalRuleSource("ns", "src")
	m, reg := newTestMetrics(t)
	m.RecordRuleSource(rs)

	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_rulesource_info"))

	m.ForgetRuleSource("ns", "src")

	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_rulesource_info"))
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_rulesource_condition"))
}

// TestCorazaMetricsRuleSourcesTotal verifies per-namespace RuleSource counts.
func TestCorazaMetricsRuleSourcesTotal(t *testing.T) {
	m, reg := newTestMetrics(t)
	m.SetRuleSourcesTotal("ns-a", 2)
	m.SetRuleSourcesTotal("ns-b", 1)

	gathered, err := reg.Gather()
	require.NoError(t, err)

	totals := make(map[string]float64)
	for _, mf := range gathered {
		if mf.GetName() != "coraza_rulesources" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			var ns string
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "namespace" {
					ns = lp.GetValue()
				}
			}
			totals[ns] = metric.GetGauge().GetValue()
		}
	}

	assert.Equal(t, float64(2), totals["ns-a"])
	assert.Equal(t, float64(1), totals["ns-b"])
}

// -----------------------------------------------------------------------------
// Tests — RuleSet
// -----------------------------------------------------------------------------

// TestCorazaMetricsRecordRuleSet verifies source count, data count, and
// condition gauges for a RuleSet.
func TestCorazaMetricsRecordRuleSet(t *testing.T) {
	rs := minimalRuleSet("default", "my-rs", 3, 2)
	rs.Status.Conditions = []metav1.Condition{
		{Type: conditionReady, Status: metav1.ConditionTrue, Reason: "r", LastTransitionTime: metav1.Now()},
	}

	m, reg := newTestMetrics(t)
	m.RecordRuleSet(rs)

	gathered, err := reg.Gather()
	require.NoError(t, err)

	metricValues := make(map[string]float64)
	condValues := make(map[string]float64)
	for _, mf := range gathered {
		switch mf.GetName() {
		case metricRuleSetSources, metricRuleSetDataFiles:
			for _, metric := range mf.GetMetric() {
				metricValues[mf.GetName()] = metric.GetGauge().GetValue()
			}
		case "coraza_ruleset_condition":
			for _, metric := range mf.GetMetric() {
				var ct string
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == labelCondition {
						ct = lp.GetValue()
					}
				}
				condValues[ct] = metric.GetGauge().GetValue()
			}
		}
	}

	assert.Equal(t, float64(3), metricValues[metricRuleSetSources])
	assert.Equal(t, float64(2), metricValues[metricRuleSetDataFiles])
	assert.Equal(t, float64(1), condValues[conditionReady])
	assert.Equal(t, float64(-1), condValues[conditionProgressing])
}

// TestCorazaMetricsRuleSetSpecChangeClearsStale verifies that when a RuleSet's
// spec.sources or spec.data counts change, the gauges are updated to the new
// values and no stale series remain.
func TestCorazaMetricsRuleSetSpecChangeClearsStale(t *testing.T) {
	m, reg := newTestMetrics(t)

	// Initial state: 3 sources, 2 data files.
	rs := minimalRuleSet("default", "my-rs", 3, 2)
	m.RecordRuleSet(rs)

	gatherMetricValues := func() (sources, dataFiles float64) {
		gathered, err := reg.Gather()
		require.NoError(t, err)
		for _, mf := range gathered {
			switch mf.GetName() {
			case metricRuleSetSources:
				for _, metric := range mf.GetMetric() {
					sources = metric.GetGauge().GetValue()
				}
			case metricRuleSetDataFiles:
				for _, metric := range mf.GetMetric() {
					dataFiles = metric.GetGauge().GetValue()
				}
			}
		}
		return
	}

	sources, dataFiles := gatherMetricValues()
	assert.Equal(t, float64(3), sources, "initial sources count must be 3")
	assert.Equal(t, float64(2), dataFiles, "initial data_files count must be 2")

	// Simulate spec change: reduce to 1 source, 0 data files.
	rs.Spec.Sources = rs.Spec.Sources[:1]
	rs.Spec.Data = nil
	m.RecordRuleSet(rs)

	sources, dataFiles = gatherMetricValues()
	assert.Equal(t, float64(1), sources, "after spec change sources count must be 1")
	assert.Equal(t, float64(0), dataFiles, "after spec change data_files count must be 0")

	// Only one info series must exist — no stale series from the first record.
	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_ruleset_info"))
}

// TestCorazaMetricsForgetRuleSet verifies that ForgetRuleSet removes all
// series for the named ruleset.
func TestCorazaMetricsForgetRuleSet(t *testing.T) {
	rs := minimalRuleSet("ns", "rs", 1, 0)
	m, reg := newTestMetrics(t)
	m.RecordRuleSet(rs)

	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_ruleset_info"))

	m.ForgetRuleSet("ns", "rs")

	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_ruleset_info"))
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_ruleset_condition"))
	assert.Equal(t, 0, testutil.CollectAndCount(reg, metricRuleSetSources))
	assert.Equal(t, 0, testutil.CollectAndCount(reg, metricRuleSetDataFiles))
}

// -----------------------------------------------------------------------------
// Tests — RuleData
// -----------------------------------------------------------------------------

// TestCorazaMetricsRecordRuleData verifies that Ready condition is emitted for
// a RuleData with a Ready=True status.
func TestCorazaMetricsRecordRuleData(t *testing.T) {
	rd := minimalRuleData("default", "my-rd")
	rd.Status = wafv1alpha1.RuleDataStatus{
		Conditions: []metav1.Condition{
			{Type: conditionReady, Status: metav1.ConditionTrue, Reason: "Loaded", LastTransitionTime: metav1.Now()},
		},
	}

	m, reg := newTestMetrics(t)
	m.RecordRuleData(rd)

	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_ruledata_info"))

	gathered, err := reg.Gather()
	require.NoError(t, err)

	var readyVal *float64
	for _, mf := range gathered {
		if mf.GetName() != "coraza_ruledata_condition" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == labelCondition && lp.GetValue() == conditionReady {
					v := metric.GetGauge().GetValue()
					readyVal = &v
				}
			}
		}
	}

	require.NotNil(t, readyVal, "coraza_ruledata_condition{condition=Ready} not found")
	assert.Equal(t, float64(1), *readyVal)
}

// TestCorazaMetricsForgetRuleData verifies that ForgetRuleData removes all
// series for the named ruledata.
func TestCorazaMetricsForgetRuleData(t *testing.T) {
	rd := minimalRuleData("ns", "rd")
	m, reg := newTestMetrics(t)
	m.RecordRuleData(rd)

	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_ruledata_info"))

	m.ForgetRuleData("ns", "rd")

	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_ruledata_info"))
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_ruledata_condition"))
}

// TestCorazaMetricsRuleDataDereferencedClearsStale verifies the "was referenced,
// now removed from spec.data" scenario: after a RuleData is recorded and then
// forgotten (simulating the reconciler detecting it is no longer in spec.data),
// all its series must be absent from the registry.
func TestCorazaMetricsRuleDataDereferencedClearsStale(t *testing.T) {
	m, reg := newTestMetrics(t)

	rd := minimalRuleData("ns", "data-rd")
	m.RecordRuleData(rd)

	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_ruledata_info"),
		"series must be present after RecordRuleData")

	// Simulate the RuleSet reconciler calling ForgetRuleData when the entry is
	// no longer in spec.data.
	m.ForgetRuleData("ns", "data-rd")

	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_ruledata_info"),
		"info series must be gone after ForgetRuleData")
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_ruledata_condition"),
		"condition series must be gone after ForgetRuleData")
}

// TestCorazaMetricsSetTotalsDeleteSeriesOnZero verifies that SetXxxTotal removes
// the namespace series entirely when count drops to zero rather than setting it
// to 0 (preventing stale accumulation over the operator lifetime).
func TestCorazaMetricsSetTotalsDeleteSeriesOnZero(t *testing.T) {
	m, reg := newTestMetrics(t)

	// Set non-zero, then drop to zero: series must vanish.
	m.SetEnginesTotal("ns-a", 2)
	m.SetEnginesTotal("ns-a", 0)
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_engines"),
		"coraza_engines series must be deleted when count drops to zero")

	m.SetRuleSetsTotal("ns-b", 3)
	m.SetRuleSetsTotal("ns-b", 0)
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_rulesets"),
		"coraza_rulesets series must be deleted when count drops to zero")

	m.SetRuleSourcesTotal("ns-c", 1)
	m.SetRuleSourcesTotal("ns-c", 0)
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_rulesources"),
		"coraza_rulesources series must be deleted when count drops to zero")

	m.SetRuleDatasTotal("ns-d", 4)
	m.SetRuleDatasTotal("ns-d", 0)
	assert.Equal(t, 0, testutil.CollectAndCount(reg, "coraza_ruledatas"),
		"coraza_ruledatas series must be deleted when count drops to zero")
}

// -----------------------------------------------------------------------------
// Tests — nil-safety
// -----------------------------------------------------------------------------

// TestCorazaMetricsNilSafe verifies that calling any method on a nil
// *CorazaMetrics does not panic.
func TestCorazaMetricsNilSafe(t *testing.T) {
	var m *CorazaMetrics
	assert.NotPanics(t, func() {
		m.RecordEngine(minimalEngine("ns", "eng", "gw"))
		m.ForgetEngine("ns", "eng")
		m.SetEnginesTotal("ns", 0)
		m.RecordRuleSet(minimalRuleSet("ns", "rs", 1, 0))
		m.ForgetRuleSet("ns", "rs")
		m.SetRuleSetsTotal("ns", 0)
		m.RecordRuleSource(minimalRuleSource("ns", "src"))
		m.ForgetRuleSource("ns", "src")
		m.SetRuleSourcesTotal("ns", 0)
		m.RecordRuleData(minimalRuleData("ns", "rd"))
		m.ForgetRuleData("ns", "rd")
		m.SetRuleDatasTotal("ns", 0)
		m.IncRuleSourceValidation("ns", "valid")
		m.IncRuleSetValidation("ns", "invalid")
		m.ObserveRuleSourceValidation("ns", "valid", time.Millisecond)
		m.ObserveRuleSetValidation("ns", "invalid", 2*time.Millisecond)
		m.ObserveCacheSet("ns", time.Microsecond)
	})
}

// TestCorazaMetricsValidationMetrics verifies validation counters and histograms.
func TestCorazaMetricsValidationMetrics(t *testing.T) {
	m, reg := newTestMetrics(t)

	m.IncRuleSourceValidation("default", "valid")
	m.IncRuleSourceValidation("default", "invalid")
	m.IncRuleSourceValidation("default", "skipped")
	m.IncRuleSetValidation("default", "valid")
	m.IncRuleSetValidation("default", "invalid")
	m.ObserveRuleSourceValidation("default", "valid", 10*time.Millisecond)
	m.ObserveRuleSetValidation("default", "valid", 20*time.Millisecond)
	m.ObserveCacheSet("default", 5*time.Millisecond)

	gathered, err := reg.Gather()
	require.NoError(t, err)

	counterValues := make(map[string]float64)
	for _, mf := range gathered {
		if mf.GetName() != "coraza_rulesource_validations_total" && mf.GetName() != "coraza_ruleset_validations_total" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			var outcome string
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "outcome" {
					outcome = lp.GetValue()
				}
			}
			counterValues[mf.GetName()+"/"+outcome] = metric.GetCounter().GetValue()
		}
	}

	assert.Equal(t, float64(1), counterValues["coraza_rulesource_validations_total/valid"])
	assert.Equal(t, float64(1), counterValues["coraza_rulesource_validations_total/invalid"])
	assert.Equal(t, float64(1), counterValues["coraza_rulesource_validations_total/skipped"])
	assert.Equal(t, float64(1), counterValues["coraza_ruleset_validations_total/valid"])
	assert.Equal(t, float64(1), counterValues["coraza_ruleset_validations_total/invalid"])

	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_rulesource_validation_duration_seconds"))
	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_ruleset_validation_duration_seconds"))
	assert.Equal(t, 1, testutil.CollectAndCount(reg, "coraza_cache_set_duration_seconds"))
}

func gatherValidationCounter(t *testing.T, reg *prometheus.Registry, outcome string) float64 {
	return gatherCounterOutcome(t, reg, "coraza_rulesource_validations_total", outcome)
}

func gatherRuleSetValidationCounter(t *testing.T, reg *prometheus.Registry, outcome string) float64 {
	return gatherCounterOutcome(t, reg, "coraza_ruleset_validations_total", outcome)
}

func gatherNamespaceGauge(t *testing.T, reg *prometheus.Registry, metricName, ns string) float64 {
	t.Helper()
	gathered, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range gathered {
		if mf.GetName() != metricName {
			continue
		}
		for _, metric := range mf.GetMetric() {
			labels := make(map[string]string)
			for _, lp := range metric.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["namespace"] == ns {
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func gatherCacheSetDurationSampleCount(t *testing.T, reg *prometheus.Registry) uint64 {
	t.Helper()
	gathered, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range gathered {
		if mf.GetName() != "coraza_cache_set_duration_seconds" {
			continue
		}
		var total uint64
		for _, metric := range mf.GetMetric() {
			if h := metric.GetHistogram(); h != nil {
				total += h.GetSampleCount()
			}
		}
		return total
	}
	return 0
}

func gatherCounterOutcome(t *testing.T, reg *prometheus.Registry, metricName, outcome string) float64 {
	t.Helper()
	gathered, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range gathered {
		if mf.GetName() != metricName {
			continue
		}
		for _, metric := range mf.GetMetric() {
			labels := make(map[string]string)
			for _, lp := range metric.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["outcome"] == outcome {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
