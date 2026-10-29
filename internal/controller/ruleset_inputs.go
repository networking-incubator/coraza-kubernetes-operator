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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

// loadRuleSetInputs resolves a RuleSet's RuleSources and RuleData into
// assembler inputs, preserving spec order and UID/generation. It performs plain
// Gets and has no status or requeue semantics.
func loadRuleSetInputs(ctx context.Context, reader client.Reader, ruleset *wafv1alpha1.RuleSet) (assemble.Inputs, error) {
	inputs := assemble.Inputs{
		RuleSetUID:       ruleset.UID,
		RuleSetName:      ruleset.Name,
		Namespace:        ruleset.Namespace,
		EmptyRulesPolicy: assemble.EmptyRulesPolicy(ruleset.Spec.EmptyRulesPolicy),
	}

	for _, ref := range ruleset.Spec.Sources {
		var source wafv1alpha1.RuleSource
		if err := reader.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: ref.Name}, &source); err != nil {
			return assemble.Inputs{}, fmt.Errorf("get RuleSource %s: %w", ref.Name, err)
		}
		inputs.Sources = append(inputs.Sources, assemble.SourceInput{
			Name:       source.Name,
			UID:        source.UID,
			Generation: source.Generation,
			Rules:      source.Spec.Rules,
		})
	}

	for _, ref := range ruleset.Spec.Data {
		var data wafv1alpha1.RuleData
		if err := reader.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: ref.Name}, &data); err != nil {
			return assemble.Inputs{}, fmt.Errorf("get RuleData %s: %w", ref.Name, err)
		}
		inputs.Data = append(inputs.Data, assemble.DataInput{
			Name:       data.Name,
			UID:        data.UID,
			Generation: data.Generation,
			Files:      data.Spec.Files,
		})
	}

	return inputs, nil
}

// engineInputsChanged reports whether the Engine, its RuleSet, or any resolved
// RuleSet input changed after they were read. A changed read must never be
// accepted as if it were one consistent configuration.
func (r *EngineReconciler) engineInputsChanged(ctx context.Context, engine *wafv1alpha1.Engine, ruleset *wafv1alpha1.RuleSet, inputs assemble.Inputs) (bool, error) {
	var currentEngine wafv1alpha1.Engine
	if err := r.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: engine.Name}, &currentEngine); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if currentEngine.UID != engine.UID || currentEngine.Generation != engine.Generation {
		return true, nil
	}

	var currentRuleSet wafv1alpha1.RuleSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: ruleset.Name}, &currentRuleSet); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if currentRuleSet.UID != ruleset.UID || currentRuleSet.Generation != ruleset.Generation {
		return true, nil
	}

	for _, input := range inputs.Sources {
		var source wafv1alpha1.RuleSource
		if err := r.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: input.Name}, &source); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		if source.UID != input.UID || source.Generation != input.Generation {
			return true, nil
		}
	}

	for _, input := range inputs.Data {
		var data wafv1alpha1.RuleData
		if err := r.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: input.Name}, &data); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		if data.UID != input.UID || data.Generation != input.Generation {
			return true, nil
		}
	}

	return false, nil
}
