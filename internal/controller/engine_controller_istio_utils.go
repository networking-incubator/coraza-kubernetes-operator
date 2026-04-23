package controller

import (
	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Istio Helpers
// -----------------------------------------------------------------------------

// hasIstioWasmDriver reports whether the Engine has a fully-specified Istio
// Wasm driver configuration.
func hasIstioWasmDriver(engine *wafv1alpha1.Engine) bool {
	return engine.Spec.Driver != nil &&
		engine.Spec.Driver.Istio != nil &&
		engine.Spec.Driver.Istio.Wasm != nil
}

// hasIstioDynamicModuleDriver reports whether the Engine has a fully-specified
// Istio dynamic module driver configuration.
func hasIstioDynamicModuleDriver(engine *wafv1alpha1.Engine) bool {
	return engine.Spec.Driver != nil &&
		engine.Spec.Driver.Istio != nil &&
		engine.Spec.Driver.Istio.DynamicModule != nil
}
