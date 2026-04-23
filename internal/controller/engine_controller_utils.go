package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Engine Helpers
// -----------------------------------------------------------------------------

// engineMatchesLabels reports whether the Engine's workload selector matches
// the given labels.
func engineMatchesLabels(engine *wafv1alpha1.Engine, podLabels map[string]string) bool {
	ws := workloadSelector(engine)
	if ws == nil {
		return false
	}

	selector, err := metav1.LabelSelectorAsSelector(ws)
	if err != nil {
		return false
	}

	return selector.Matches(labels.Set(podLabels))
}
