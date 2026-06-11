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
	"github.com/prometheus/client_golang/prometheus"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Condition type lists
// -----------------------------------------------------------------------------

// Condition types tracked per resource kind. These must match the types
// actually set by each reconciler so that ForgetXxx can clean up all series.
var (
	engineConditionTypes     = []string{conditionReady, conditionProgressing, conditionDegraded, conditionAccepted}
	rulesetConditionTypes    = []string{conditionReady, conditionProgressing, conditionDegraded}
	rulesourceConditionTypes = []string{conditionReady, conditionDegraded}
	ruledataConditionTypes   = []string{conditionReady}
)

// -----------------------------------------------------------------------------
// CorazaMetrics
// -----------------------------------------------------------------------------

// CorazaMetrics holds all Prometheus gauge metrics for the Coraza operator.
// Gauges are updated during reconciliation rather than at scrape time, so
// there are no live client.List calls on the Prometheus /metrics endpoint.
//
// All exported methods are nil-safe: calling them on a nil *CorazaMetrics is
// a no-op. This lets reconcilers reference an uninitialized metrics instance
// in tests without requiring a registry.
type CorazaMetrics struct {
	engineInfo      *prometheus.GaugeVec
	engineCondition *prometheus.GaugeVec
	engines         *prometheus.GaugeVec

	rulesetInfo      *prometheus.GaugeVec
	rulesetCondition *prometheus.GaugeVec
	rulesets         *prometheus.GaugeVec
	rulesetSources   *prometheus.GaugeVec
	rulesetDataFiles *prometheus.GaugeVec

	rulesourceInfo      *prometheus.GaugeVec
	rulesourceCondition *prometheus.GaugeVec
	rulesources         *prometheus.GaugeVec

	ruledataInfo      *prometheus.GaugeVec
	ruledataCondition *prometheus.GaugeVec
	ruledatas         *prometheus.GaugeVec
}

// NewCorazaMetrics creates a CorazaMetrics and registers all gauges with reg.
// Returns an error if any gauge conflicts with an already-registered metric.
func NewCorazaMetrics(reg prometheus.Registerer) (*CorazaMetrics, error) {
	m := &CorazaMetrics{
		engineInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_engine_info",
			Help: "Information about a Coraza Engine resource. Value is always 1.",
		}, []string{"namespace", "name", "target_name", "target_type", "driver_type", "ruleset_name", "failure_policy"}),

		engineCondition: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_engine_condition",
			Help: "Current state of an Engine condition. 1=True 0=False -1=Unknown.",
		}, []string{"namespace", "name", "condition"}),

		engines: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_engines",
			Help: "Total number of Engine resources per namespace.",
		}, []string{"namespace"}),

		rulesetInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_ruleset_info",
			Help: "Information about a Coraza RuleSet resource. Value is always 1.",
		}, []string{"namespace", "name"}),

		rulesetCondition: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_ruleset_condition",
			Help: "Current state of a RuleSet condition. 1=True 0=False -1=Unknown.",
		}, []string{"namespace", "name", "condition"}),

		rulesets: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_rulesets",
			Help: "Total number of RuleSet resources per namespace.",
		}, []string{"namespace"}),

		rulesetSources: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_ruleset_sources",
			Help: "Number of RuleSources referenced by the RuleSet.",
		}, []string{"namespace", "name"}),

		rulesetDataFiles: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_ruleset_data_files",
			Help: "Number of RuleData resources referenced by the RuleSet.",
		}, []string{"namespace", "name"}),

		rulesourceInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_rulesource_info",
			Help: "Information about a Coraza RuleSource resource. Value is always 1.",
		}, []string{"namespace", "name"}),

		rulesourceCondition: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_rulesource_condition",
			Help: "Current state of a RuleSource condition. 1=True 0=False -1=Unknown.",
		}, []string{"namespace", "name", "condition"}),

		rulesources: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_rulesources",
			Help: "Total number of RuleSource resources per namespace.",
		}, []string{"namespace"}),

		ruledataInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_ruledata_info",
			Help: "Information about a Coraza RuleData resource. Value is always 1.",
		}, []string{"namespace", "name"}),

		ruledataCondition: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_ruledata_condition",
			Help: "Current state of a RuleData condition. 1=True 0=False -1=Unknown.",
		}, []string{"namespace", "name", "condition"}),

		ruledatas: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_ruledatas",
			Help: "Total number of RuleData resources per namespace.",
		}, []string{"namespace"}),
	}

	for _, c := range []prometheus.Collector{
		m.engineInfo, m.engineCondition, m.engines,
		m.rulesetInfo, m.rulesetCondition, m.rulesets, m.rulesetSources, m.rulesetDataFiles,
		m.rulesourceInfo, m.rulesourceCondition, m.rulesources,
		m.ruledataInfo, m.ruledataCondition, m.ruledatas,
	} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// conditionStatusToFloat converts a metav1.ConditionStatus to its numeric
// representation: True=1, False=0, Unknown=-1.
func conditionStatusToFloat(status metav1.ConditionStatus) float64 {
	switch status {
	case metav1.ConditionTrue:
		return 1
	case metav1.ConditionFalse:
		return 0
	default:
		return -1
	}
}

func setConditions(vec *prometheus.GaugeVec, ns, name string, conditions []metav1.Condition, types []string) {
	for _, ct := range types {
		val := float64(-1)
		if cond := apimeta.FindStatusCondition(conditions, ct); cond != nil {
			val = conditionStatusToFloat(cond.Status)
		}
		vec.WithLabelValues(ns, name, ct).Set(val)
	}
}

// -----------------------------------------------------------------------------
// Engine
// -----------------------------------------------------------------------------

// RecordEngine updates all Engine gauges for the given engine. It first
// removes any stale info series (spec label values can change on spec updates)
// then re-emits with current values. All condition types are always emitted;
// absent conditions produce -1.
func (m *CorazaMetrics) RecordEngine(e *wafv1alpha1.Engine) {
	if m == nil {
		return
	}

	// Delete old info series before re-emitting so spec-label changes
	// (target, ruleset, failure_policy) do not leave stale series.
	m.engineInfo.DeletePartialMatch(prometheus.Labels{"namespace": e.Namespace, "name": e.Name})

	dt := string(e.Spec.Driver.Type)
	if dt == "" {
		dt = string(wafv1alpha1.DriverTypeWasm)
	}
	m.engineInfo.WithLabelValues(
		e.Namespace, e.Name,
		e.Spec.Target.Name, string(e.Spec.Target.Type),
		dt, e.Spec.RuleSet.Name, string(e.Spec.FailurePolicy),
	).Set(1)

	var conds []metav1.Condition
	if e.Status != nil {
		conds = e.Status.Conditions
	}
	setConditions(m.engineCondition, e.Namespace, e.Name, conds, engineConditionTypes)
}

// ForgetEngine removes all gauge series for the given engine.
func (m *CorazaMetrics) ForgetEngine(ns, name string) {
	if m == nil {
		return
	}
	m.engineInfo.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})
	m.engineCondition.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})
}

// SetEnginesTotal sets the per-namespace Engine count gauge.
// When count reaches zero the series is deleted entirely so that namespaces
// with no resources do not accumulate stale zero-value series over the
// operator's lifetime.
func (m *CorazaMetrics) SetEnginesTotal(ns string, count int) {
	if m == nil {
		return
	}
	if count == 0 {
		m.engines.DeleteLabelValues(ns)
		return
	}
	m.engines.WithLabelValues(ns).Set(float64(count))
}

// -----------------------------------------------------------------------------
// RuleSet
// -----------------------------------------------------------------------------

// RecordRuleSet updates all RuleSet gauges for the given ruleset.
func (m *CorazaMetrics) RecordRuleSet(rs *wafv1alpha1.RuleSet) {
	if m == nil {
		return
	}
	m.rulesetInfo.WithLabelValues(rs.Namespace, rs.Name).Set(1)
	m.rulesetSources.WithLabelValues(rs.Namespace, rs.Name).Set(float64(len(rs.Spec.Sources)))
	m.rulesetDataFiles.WithLabelValues(rs.Namespace, rs.Name).Set(float64(len(rs.Spec.Data)))
	setConditions(m.rulesetCondition, rs.Namespace, rs.Name, rs.Status.Conditions, rulesetConditionTypes)
}

// ForgetRuleSet removes all gauge series for the given ruleset.
func (m *CorazaMetrics) ForgetRuleSet(ns, name string) {
	if m == nil {
		return
	}
	m.rulesetInfo.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})
	m.rulesetCondition.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})
	m.rulesetSources.DeleteLabelValues(ns, name)
	m.rulesetDataFiles.DeleteLabelValues(ns, name)
}

// SetRuleSetsTotal sets the per-namespace RuleSet count gauge.
// When count reaches zero the series is deleted to prevent stale accumulation.
func (m *CorazaMetrics) SetRuleSetsTotal(ns string, count int) {
	if m == nil {
		return
	}
	if count == 0 {
		m.rulesets.DeleteLabelValues(ns)
		return
	}
	m.rulesets.WithLabelValues(ns).Set(float64(count))
}

// -----------------------------------------------------------------------------
// RuleSource
// -----------------------------------------------------------------------------

// RecordRuleSource updates all RuleSource gauges for the given rulesource.
func (m *CorazaMetrics) RecordRuleSource(rs *wafv1alpha1.RuleSource) {
	if m == nil {
		return
	}
	m.rulesourceInfo.WithLabelValues(rs.Namespace, rs.Name).Set(1)
	setConditions(m.rulesourceCondition, rs.Namespace, rs.Name, rs.Status.Conditions, rulesourceConditionTypes)
}

// ForgetRuleSource removes all gauge series for the given rulesource.
func (m *CorazaMetrics) ForgetRuleSource(ns, name string) {
	if m == nil {
		return
	}
	m.rulesourceInfo.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})
	m.rulesourceCondition.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})
}

// SetRuleSourcesTotal sets the per-namespace RuleSource count gauge.
// When count reaches zero the series is deleted to prevent stale accumulation.
func (m *CorazaMetrics) SetRuleSourcesTotal(ns string, count int) {
	if m == nil {
		return
	}
	if count == 0 {
		m.rulesources.DeleteLabelValues(ns)
		return
	}
	m.rulesources.WithLabelValues(ns).Set(float64(count))
}

// -----------------------------------------------------------------------------
// RuleData
// -----------------------------------------------------------------------------

// RecordRuleData updates all RuleData gauges for the given ruledata.
// RuleData metrics are emitted only for objects referenced by at least one
// RuleSet, since there is no dedicated RuleData reconciler.
func (m *CorazaMetrics) RecordRuleData(rd *wafv1alpha1.RuleData) {
	if m == nil {
		return
	}
	m.ruledataInfo.WithLabelValues(rd.Namespace, rd.Name).Set(1)
	setConditions(m.ruledataCondition, rd.Namespace, rd.Name, rd.Status.Conditions, ruledataConditionTypes)
}

// ForgetRuleData removes all gauge series for the given ruledata.
func (m *CorazaMetrics) ForgetRuleData(ns, name string) {
	if m == nil {
		return
	}
	m.ruledataInfo.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})
	m.ruledataCondition.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})
}

// SetRuleDatasTotal sets the per-namespace RuleData count gauge.
// When count reaches zero the series is deleted to prevent stale accumulation.
func (m *CorazaMetrics) SetRuleDatasTotal(ns string, count int) {
	if m == nil {
		return
	}
	if count == 0 {
		m.ruledatas.DeleteLabelValues(ns)
		return
	}
	m.ruledatas.WithLabelValues(ns).Set(float64(count))
}
