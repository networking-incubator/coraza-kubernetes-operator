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

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
)

// -----------------------------------------------------------------------------
// Global RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="coordination.k8s.io",resources=leases,verbs=get;list;watch;create;update;patch;delete

// -----------------------------------------------------------------------------
// Manager - Vars
// -----------------------------------------------------------------------------

// DefaultRuleSetCacheServerPort is the default port number for the RuleSet
// cache server.
const DefaultRuleSetCacheServerPort = 18080

// -----------------------------------------------------------------------------
// Manager - Setup
// -----------------------------------------------------------------------------

// SetupControllers initializes all controllers
func SetupControllers(mgr ctrl.Manager, rulesetCache *cache.RuleSetCache, envoyClusterName string) error {
	if err := (&RuleSetReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("ruleset-controller"),
		Cache:    rulesetCache,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller RuleSet: %w", err)
	}

	if err := (&EngineReconciler{
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		Recorder:                  mgr.GetEventRecorder("engine-controller"),
		ruleSetCacheServerCluster: envoyClusterName,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller Engine: %w", err)
	}

	return nil
}
