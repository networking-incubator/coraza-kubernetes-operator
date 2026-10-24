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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Legacy Engine Preflight
// -----------------------------------------------------------------------------

const (
	// ReasonLegacyWasmImageFields is the condition and event reason used for
	// Engines that still set the removed Wasm image fields.
	ReasonLegacyWasmImageFields = "LegacyWasmImageFields"

	legacyWasmImageFieldsMessage = "Engine uses removed spec.driver.wasm.image/imagePullSecret; set url and sha256, or omit both to use the bundled module"
)

// LegacyEnginePreflight lists stored Engines through the uncached API reader and
// reports those that still set the removed spec.driver.wasm.image or
// imagePullSecret fields. It runs on every replica at startup.
type LegacyEnginePreflight struct {
	// Reader must be the manager's APIReader so legacy objects are read
	// uncached and decoded without the typed API.
	Reader client.Reader

	// Recorder emits a Warning event on each offending Engine. Optional.
	Recorder events.EventRecorder

	// Metrics optionally records the number of offending Engines.
	Metrics *CorazaMetrics

	// FailOnLegacy makes Start return an error when any offending Engine is
	// found, stopping the manager.
	FailOnLegacy bool
}

// NeedLeaderElection reports that the preflight runs on every replica.
func (p *LegacyEnginePreflight) NeedLeaderElection() bool { return false }

// Start lists Engines once and reports any that use the removed Wasm image
// fields.
func (p *LegacyEnginePreflight) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("legacy-preflight")

	engines := &unstructured.UnstructuredList{}
	engines.SetGroupVersionKind(wafv1alpha1.GroupVersion.WithKind("EngineList"))
	if err := p.Reader.List(ctx, engines); err != nil {
		return fmt.Errorf("list Engines for legacy preflight: %w", err)
	}

	count := 0
	for i := range engines.Items {
		engine := &engines.Items[i]
		imageSet, imagePullSecretSet := legacyWasmFields(engine)
		if !imageSet && !imagePullSecretSet {
			continue
		}

		count++
		logger.Info("Detected Engine using removed Wasm image fields",
			"namespace", engine.GetNamespace(),
			"name", engine.GetName(),
			"imageSet", imageSet,
			"imagePullSecretSet", imagePullSecretSet)

		if p.Recorder != nil {
			engine.SetGroupVersionKind(wafv1alpha1.GroupVersion.WithKind("Engine"))
			p.Recorder.Eventf(engine, nil, corev1.EventTypeWarning,
				ReasonLegacyWasmImageFields, "Preflight", truncateEventNote(legacyWasmImageFieldsMessage))
		}
	}

	if p.Metrics != nil {
		p.Metrics.SetLegacyWasmEngines(count)
	}

	if count == 0 {
		logger.Info("Legacy Engine preflight completed", "legacyEngines", 0)
		return nil
	}

	if p.FailOnLegacy {
		return fmt.Errorf("%d Engine(s) use removed spec.driver.wasm.image/imagePullSecret; migrate to url and sha256, or omit both to use the bundled module", count)
	}

	logger.Info("Legacy Engine preflight completed", "legacyEngines", count)
	return nil
}

// hasLegacyWasmFields reads the raw stored Engine and reports whether it sets
// either removed Wasm image field. The object is read as unstructured so typed
// decoding cannot hide fields that the current CRD no longer accepts.
func hasLegacyWasmFields(ctx context.Context, reader client.Reader, key client.ObjectKey) (bool, error) {
	engine := &unstructured.Unstructured{}
	engine.SetGroupVersionKind(wafv1alpha1.GroupVersion.WithKind("Engine"))
	if err := reader.Get(ctx, key, engine); err != nil {
		return false, err
	}
	imageSet, imagePullSecretSet := legacyWasmFields(engine)
	return imageSet || imagePullSecretSet, nil
}

// legacyWasmFields reports whether the unstructured Engine sets the removed
// image and imagePullSecret fields. Fields are detected by presence so that
// objects stored with the old schema are not hidden by typed decoding.
func legacyWasmFields(engine *unstructured.Unstructured) (bool, bool) {
	_, imageSet, _ := unstructured.NestedString(engine.Object, "spec", "driver", "wasm", "image")
	_, imagePullSecretSet, _ := unstructured.NestedString(engine.Object, "spec", "driver", "wasm", "imagePullSecret")
	return imageSet, imagePullSecretSet
}
