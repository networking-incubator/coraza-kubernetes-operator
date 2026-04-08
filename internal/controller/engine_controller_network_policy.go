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

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Engine Controller - NetworkPolicy RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=networking.k8s.io,namespace=system,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// -----------------------------------------------------------------------------
// Engine Controller - NetworkPolicy Constants
// -----------------------------------------------------------------------------

const (
	// networkPolicyFinalizer is added to Engine resources to guarantee that the
	// cross-namespace NetworkPolicy is deleted before the Engine is removed.
	// Without this, the Engine and NetworkPolicy live in different namespaces so
	// ownerReference-based garbage collection cannot be used.
	networkPolicyFinalizer = "waf.k8s.coraza.io/network-policy-cleanup"

	// NetworkPolicyGenerateName is the prefix used with GenerateName for all
	// created NetworkPolicy resources.
	NetworkPolicyGenerateName = "coraza-cache-"

	// operatorPodLabelKey and operatorPodLabelValue identify the operator pods
	// targeted by the cache server NetworkPolicy. These match the labels set by
	// the Helm chart's selectorLabels template.
	operatorPodLabelKey   = "control-plane"
	operatorPodLabelValue = "coraza-controller-manager"

	// networkPolicyEngineLabelName and networkPolicyEngineLabelNamespace are the
	// label keys used to associate a NetworkPolicy with its owning Engine.
	networkPolicyEngineLabelName      = "waf.k8s.coraza.io/engine-name"
	networkPolicyEngineLabelNamespace = "waf.k8s.coraza.io/engine-namespace"
)

// -----------------------------------------------------------------------------
// Engine Controller - NetworkPolicy Lookup
// -----------------------------------------------------------------------------

// engineNetworkPolicyLabels returns the label selector used to find the
// NetworkPolicy owned by a given Engine.
func engineNetworkPolicyLabels(engineNamespace, engineName string) client.MatchingLabels {
	return client.MatchingLabels{
		networkPolicyEngineLabelName:      engineName,
		networkPolicyEngineLabelNamespace: engineNamespace,
	}
}

// findNetworkPolicyForEngine lists NetworkPolicies in the operator namespace
// that belong to the given Engine. Returns the first match or nil.
func (r *EngineReconciler) findNetworkPolicyForEngine(ctx context.Context, engineNamespace, engineName string) (*networkingv1.NetworkPolicy, error) {
	var list networkingv1.NetworkPolicyList
	if err := r.List(ctx, &list,
		client.InNamespace(r.operatorNamespace),
		engineNetworkPolicyLabels(engineNamespace, engineName),
	); err != nil {
		return nil, fmt.Errorf("list NetworkPolicies: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &list.Items[0], nil
}

// -----------------------------------------------------------------------------
// Engine Controller - NetworkPolicy Finalizer
// -----------------------------------------------------------------------------

// ensureNetworkPolicyFinalizer adds the finalizer to the Engine if it is not
// already present. Returns true if the Engine was patched.
func (r *EngineReconciler) ensureNetworkPolicyFinalizer(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) (bool, error) {
	if controllerutil.ContainsFinalizer(engine, networkPolicyFinalizer) {
		return false, nil
	}

	patch := client.MergeFrom(engine.DeepCopy())
	controllerutil.AddFinalizer(engine, networkPolicyFinalizer)
	if err := r.Patch(ctx, engine, patch); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to add NetworkPolicy finalizer", engine)
		return false, err
	}
	logDebug(log, req, "Engine", "Added NetworkPolicy finalizer")
	return true, nil
}

// handleNetworkPolicyDeletion cleans up the NetworkPolicy and removes the
// finalizer so the Engine can be garbage-collected. Returns true when the
// Engine is being deleted and the caller should stop reconciling.
func (r *EngineReconciler) handleNetworkPolicyDeletion(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) (bool, error) {
	if engine.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if !controllerutil.ContainsFinalizer(engine, networkPolicyFinalizer) {
		return true, nil
	}

	if err := r.cleanupNetworkPolicy(ctx, log, req); err != nil {
		return true, err
	}

	patch := client.MergeFrom(engine.DeepCopy())
	controllerutil.RemoveFinalizer(engine, networkPolicyFinalizer)
	if err := r.Patch(ctx, engine, patch); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to remove NetworkPolicy finalizer", engine)
		return true, err
	}
	logDebug(log, req, "Engine", "Removed NetworkPolicy finalizer")
	return true, nil
}

// -----------------------------------------------------------------------------
// Engine Controller - NetworkPolicy Apply / Delete
// -----------------------------------------------------------------------------

// applyNetworkPolicy creates or updates a NetworkPolicy in the operator namespace
// that allows ingress from the Engine's gateway pods to the cache server port.
func (r *EngineReconciler) applyNetworkPolicy(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) error {
	ws := workloadSelector(engine)
	if ws == nil || (len(ws.MatchLabels) == 0 && len(ws.MatchExpressions) == 0) {
		return fmt.Errorf("workload selector with at least one match criterion is required for NetworkPolicy creation")
	}

	existing, err := r.findNetworkPolicyForEngine(ctx, engine.Namespace, engine.Name)
	if err != nil {
		return err
	}

	desired := r.buildNetworkPolicy(engine)

	if existing != nil {
		// Update the existing NetworkPolicy in place.
		desired.Name = existing.Name
		desired.GenerateName = ""
		desired.ResourceVersion = existing.ResourceVersion
		logDebug(log, req, "Engine", "Updating cache server NetworkPolicy", "networkPolicyName", existing.Name)
		if err := r.Update(ctx, desired); err != nil {
			logAPIError(log, req, "Engine", err, "Failed to update NetworkPolicy", desired)
			return err
		}
		logInfo(log, req, "Engine", "NetworkPolicy updated", "networkPolicyName", desired.Name, "networkPolicyNamespace", desired.Namespace)
		return nil
	}

	logDebug(log, req, "Engine", "Creating cache server NetworkPolicy")
	if err := r.Create(ctx, desired); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to create NetworkPolicy", desired)
		return err
	}
	logInfo(log, req, "Engine", "NetworkPolicy created", "networkPolicyName", desired.Name, "networkPolicyNamespace", desired.Namespace)
	return nil
}

// cleanupNetworkPolicy removes NetworkPolicies associated with an Engine.
func (r *EngineReconciler) cleanupNetworkPolicy(ctx context.Context, log logr.Logger, req ctrl.Request) error {
	var list networkingv1.NetworkPolicyList
	if err := r.List(ctx, &list,
		client.InNamespace(r.operatorNamespace),
		engineNetworkPolicyLabels(req.Namespace, req.Name),
	); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to list NetworkPolicies for cleanup", nil,
			"operatorNamespace", r.operatorNamespace)
		return err
	}

	for i := range list.Items {
		if err := r.Delete(ctx, &list.Items[i]); err != nil {
			if client.IgnoreNotFound(err) != nil {
				logAPIError(log, req, "Engine", err, "Failed to cleanup NetworkPolicy", &list.Items[i])
				return err
			}
		} else {
			logInfo(log, req, "Engine", "NetworkPolicy cleaned up", "networkPolicyName", list.Items[i].Name)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Engine Controller - NetworkPolicy Builder
// -----------------------------------------------------------------------------

// workloadSelector returns the Engine's workload selector, or nil if the
// driver chain is not fully configured.
func workloadSelector(engine *wafv1alpha1.Engine) *metav1.LabelSelector {
	if engine.Spec.Driver == nil || engine.Spec.Driver.Istio == nil ||
		engine.Spec.Driver.Istio.Wasm == nil {
		return nil
	}
	return engine.Spec.Driver.Istio.Wasm.WorkloadSelector
}

func (r *EngineReconciler) buildNetworkPolicy(engine *wafv1alpha1.Engine) *networkingv1.NetworkPolicy {
	protocol := corev1.ProtocolTCP
	port := intstr.FromInt32(int32(DefaultRuleSetCacheServerPort))

	// Deep-copy the Engine workload selector (including MatchLabels and
	// MatchExpressions) so the NetworkPolicy restricts ingress to the workloads
	// selected by the Engine configuration. Do not assume this exactly matches
	// WasmPlugin selector semantics unless that builder also preserves the full
	// LabelSelector.
	podSelector := workloadSelector(engine).DeepCopy()

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: NetworkPolicyGenerateName,
			Namespace:    r.operatorNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":    "coraza-kubernetes-operator",
				networkPolicyEngineLabelName:      engine.Name,
				networkPolicyEngineLabelNamespace: engine.Namespace,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					operatorPodLabelKey: operatorPodLabelValue,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"kubernetes.io/metadata.name": engine.Namespace,
								},
							},
							PodSelector: podSelector,
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &protocol,
							Port:     &port,
						},
					},
				},
			},
		},
	}
}

// -----------------------------------------------------------------------------
// Engine Controller - NetworkPolicy Watch Predicate
// -----------------------------------------------------------------------------

// networkPolicyPredicate filters NetworkPolicy watch events to reconcile on:
//   - Create and Delete of operator-managed policies
//   - Update when the spec changes (generation increments), enabling drift detection
//
// Managedfields-only updates (from server-side-apply) don't increment generation,
// so they won't trigger reconciles, preventing reconcile loops.
func networkPolicyPredicate() predicate.Predicate {
	hasLabel := func(obj client.Object) bool {
		_, ok := obj.GetLabels()[networkPolicyEngineLabelName]
		return ok
	}

	return predicate.And(
		predicate.NewPredicateFuncs(hasLabel),
		predicate.Or(
			predicate.Funcs{
				CreateFunc:  func(event.CreateEvent) bool { return true },
				DeleteFunc:  func(event.DeleteEvent) bool { return true },
				UpdateFunc:  func(event.UpdateEvent) bool { return false },
				GenericFunc: func(event.GenericEvent) bool { return false },
			},
			predicate.GenerationChangedPredicate{},
		),
	)
}

// -----------------------------------------------------------------------------
// Engine Controller - NetworkPolicy Watch Mapper
// -----------------------------------------------------------------------------

func (r *EngineReconciler) findEnginesForNetworkPolicy(_ context.Context, obj client.Object) []ctrl.Request {
	labels := obj.GetLabels()
	name := labels[networkPolicyEngineLabelName]
	ns := labels[networkPolicyEngineLabelNamespace]
	if name == "" || ns == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}}
}
