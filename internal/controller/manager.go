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

// Package controller implements Kubernetes controllers for WAF resources.
package controller

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
)

// -----------------------------------------------------------------------------
// Global RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="coordination.k8s.io",resources=leases,verbs=get;list;watch;create;update;patch;delete

// Metrics endpoint authentication/authorization (filters.WithAuthenticationAndAuthorization)
// requires the controller ServiceAccount to perform delegated authn/authz checks.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// -----------------------------------------------------------------------------
// Manager - Vars
// -----------------------------------------------------------------------------

// DefaultRuleSetCacheServerPort is the default port number for the RuleSet
// cache server.
const DefaultRuleSetCacheServerPort = 18080

// -----------------------------------------------------------------------------
// Manager - Setup
// -----------------------------------------------------------------------------

// RuleSetOpts holds configuration for the RuleSet reconciler.
type RuleSetOpts struct {
	// MaxPayloadSize is the per-RuleSet aggregated payload limit in bytes before
	// the reconciler rejects caching and marks the RuleSet Degraded.
	// Zero means use cache.CacheMaxSize (the same default as --cache-max-size).
	// Negative values are invalid. SetupControllers applies this default; constructing
	// RuleSetReconciler with MaxPayloadSize 0 disables the check (tests only).
	MaxPayloadSize int
}

// EngineOpts holds configuration for the Engine reconciler.
type EngineOpts struct {
	EnvoyClusterName  string
	IstioRevision     string
	DefaultWasmImage  string
	OperatorNamespace string
}

// resolveRuleSetMaxPayloadSize maps RuleSetOpts.MaxPayloadSize to the value stored
// on RuleSetReconciler. Zero opts means the default admission budget (cache.CacheMaxSize).
func resolveRuleSetMaxPayloadSize(opts RuleSetOpts) (int, error) {
	n := opts.MaxPayloadSize
	if n == 0 {
		return cache.CacheMaxSize, nil
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid RuleSetOpts.MaxPayloadSize %d: must be >= 0 (0 uses default %d bytes)", n, cache.CacheMaxSize)
	}
	return n, nil
}

// SetupControllers initializes all controllers.
func SetupControllers(mgr ctrl.Manager, kubeClient kubernetes.Interface, rulesetCache *cache.RuleSetCache, ruleSetOpts RuleSetOpts, engineOpts EngineOpts) error {
	maxPayload, err := resolveRuleSetMaxPayloadSize(ruleSetOpts)
	if err != nil {
		return err
	}

	if err := (&RuleSetReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Recorder:       mgr.GetEventRecorder("ruleset-controller"),
		Cache:          rulesetCache,
		MaxPayloadSize: maxPayload,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller RuleSet: %w", err)
	}

	if err := (&EngineReconciler{
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		Recorder:                  mgr.GetEventRecorder("engine-controller"),
		kubeClient:                kubeClient,
		ruleSetCacheServerCluster: engineOpts.EnvoyClusterName,
		istioRevision:             engineOpts.IstioRevision,
		defaultWasmImage:          engineOpts.DefaultWasmImage,
		operatorNamespace:         engineOpts.OperatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller Engine: %w", err)
	}

	return nil
}
