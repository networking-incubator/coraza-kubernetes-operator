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

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

func TestFindDuplicateReferences(t *testing.T) {
	t.Run("no duplicates returns empty string", func(t *testing.T) {
		rs := &wafv1alpha1.RuleSet{}
		rs.Spec.Sources = []wafv1alpha1.SourceReference{
			{Name: "a"},
			{Name: "b"},
		}
		rs.Spec.Data = []wafv1alpha1.DataReference{
			{Name: "x"},
			{Name: "y"},
		}
		assert.Empty(t, findDuplicateReferences(rs))
	})

	t.Run("duplicate sources detected", func(t *testing.T) {
		rs := &wafv1alpha1.RuleSet{}
		rs.Spec.Sources = []wafv1alpha1.SourceReference{
			{Name: "a"},
			{Name: "a"},
		}
		msg := findDuplicateReferences(rs)
		assert.Contains(t, msg, "spec.sources")
		assert.Contains(t, msg, "a")
	})

	t.Run("duplicate data detected", func(t *testing.T) {
		rs := &wafv1alpha1.RuleSet{}
		rs.Spec.Sources = []wafv1alpha1.SourceReference{
			{Name: "a"},
		}
		rs.Spec.Data = []wafv1alpha1.DataReference{
			{Name: "x"},
			{Name: "x"},
		}
		msg := findDuplicateReferences(rs)
		assert.Contains(t, msg, "spec.data")
		assert.Contains(t, msg, "x")
	})

	t.Run("both sources and data duplicated", func(t *testing.T) {
		rs := &wafv1alpha1.RuleSet{}
		rs.Spec.Sources = []wafv1alpha1.SourceReference{
			{Name: "a"},
			{Name: "a"},
		}
		rs.Spec.Data = []wafv1alpha1.DataReference{
			{Name: "x"},
			{Name: "x"},
		}
		msg := findDuplicateReferences(rs)
		assert.Contains(t, msg, "spec.sources", "should mention sources")
		assert.Contains(t, msg, "spec.data", "should mention data")
	})

	t.Run("empty spec returns empty string", func(t *testing.T) {
		rs := &wafv1alpha1.RuleSet{}
		assert.Empty(t, findDuplicateReferences(rs))
	})
}
