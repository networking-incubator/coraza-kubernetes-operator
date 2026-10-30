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

package xds

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

const (
	// GatewayNameLabel is the well-known label Gateway data-plane pods carry.
	// It is exported so the manager can scope the Pod informer with it.
	GatewayNameLabel  = "gateway.networking.k8s.io/gateway-name"
	conditionAccepted = "Accepted"
)

// GatewayPodSelector scopes the operator's Pod informer to Gateway
// data-plane pods. Both controllers only ever map pods carrying
// GatewayNameLabel, so caching any other pod is pure memory cost.
func GatewayPodSelector() labels.Selector {
	requirement, err := labels.NewRequirement(GatewayNameLabel, selection.Exists, nil)
	if err != nil {
		// Unreachable: a constant valid key with the Exists operator.
		panic(err)
	}
	return labels.NewSelector().Add(*requirement)
}

// GatewayPodTransform is the informer transform for the Pod cache. It drops
// the churny remainder of PodStatus (conditions, container statuses, IPs)
// and keeps Phase, because pod authorization ignores pods that are not
// Running. It also strips managed fields: a per-object Transform replaces
// the cache DefaultTransform.
func GatewayPodTransform(object any) (any, error) {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return object, nil
	}
	trimmed := pod.DeepCopy()
	trimmed.ManagedFields = nil
	trimmed.Status = corev1.PodStatus{Phase: pod.Status.Phase}
	return trimmed, nil
}

// AuthzEntry is one Engine's authorization mapping.
type AuthzEntry struct {
	ResourceName string
	EngineUID    types.UID
	Namespace    string
	EngineName   string
	GatewayName  string
	// Identities maps "namespace/name" of a Gateway workload ServiceAccount to
	// its UID. An empty UID means the UID could not be resolved at refresh.
	Identities  map[string]string
	RefreshedAt time.Time
}

// RevokeFunc is called when a resource's authorization must be revoked
// immediately; it closes the resource's live streams. NewDependencies wires
// StreamRegistry.RevokeResource.
type RevokeFunc func(resourceName string)

// AuthzIndex maps Engine resource names to the ServiceAccounts of the pods
// backing the Engine's target Gateway.
type AuthzIndex struct {
	reader          client.Reader
	serviceAccounts client.Reader
	staleness       time.Duration
	now             func() time.Time

	// Logger receives refresh failures from the periodic sweep. Defaults to
	// discarding.
	Logger logr.Logger

	mu      sync.RWMutex
	entries map[string]AuthzEntry
	known   map[string]types.NamespacedName
	// revocations counts revocations per resource name. A refresh may only
	// write while the count is unchanged since it gathered its evidence, so
	// a revocation cannot be resurrected by a concurrent or stale-snapshot
	// refresh. Counters are never removed: one integer per resource the
	// replica has ever revoked, and removing one would reopen the race it
	// closes.
	revocations map[string]uint64

	// revoke closes live streams on revocation. It is immutable after
	// construction, so it is read without holding mu.
	revoke RevokeFunc
}

// NewAuthzIndex returns an empty index. reader should be the manager's cached
// client so mapping refreshes do not hammer the API server. serviceAccounts
// resolves ServiceAccount UIDs and should be a direct (uncached) reader so the
// operator does not cache every ServiceAccount in the cluster; nil falls back
// to reader. revoke is called on every revocation; nil disables immediate
// stream closure.
func NewAuthzIndex(reader, serviceAccounts client.Reader, staleness time.Duration, revoke RevokeFunc) *AuthzIndex {
	if staleness <= 0 {
		staleness = DefaultAuthorizationMaxStaleness
	}
	if serviceAccounts == nil {
		serviceAccounts = reader
	}
	if revoke == nil {
		revoke = func(string) {}
	}
	return &AuthzIndex{
		reader:          reader,
		serviceAccounts: serviceAccounts,
		staleness:       staleness,
		now:             time.Now,
		Logger:          logr.Discard(),
		entries:         make(map[string]AuthzEntry),
		known:           make(map[string]types.NamespacedName),
		revocations:     make(map[string]uint64),
		revoke:          revoke,
	}
}

// Refresh recomputes the mapping for one Engine. An Engine that is deleting,
// not accepted for its current generation, or not targeting a live Gateway is
// revoked. A transient API failure keeps the previous mapping until it exceeds
// the staleness bound, then revokes it.
//
// The caller's engine object is the evidence: a revocation that happened
// before this call does not block it (a re-accepted Engine must become
// authorized again), but a revocation that happens during it wins and the
// write is dropped.
func (a *AuthzIndex) Refresh(ctx context.Context, engine *wafv1alpha1.Engine) error {
	if engine == nil {
		return nil
	}
	resourceName := ResourceName(engine.Namespace, engine.Name, engine.UID)
	a.mu.RLock()
	generation := a.revocations[resourceName]
	a.mu.RUnlock()
	return a.refresh(ctx, engine, resourceName, generation)
}

func (a *AuthzIndex) refresh(ctx context.Context, engine *wafv1alpha1.Engine, resourceName string, generation uint64) error {
	a.mu.Lock()
	if a.revocations[resourceName] != generation {
		a.mu.Unlock()
		return nil
	}
	a.known[resourceName] = types.NamespacedName{Namespace: engine.Namespace, Name: engine.Name}
	a.mu.Unlock()

	if !engine.DeletionTimestamp.IsZero() || !engineAcceptedForGeneration(engine) {
		a.RevokeAndNotify(resourceName)
		return nil
	}
	if engine.Spec.Target.Type != wafv1alpha1.EngineTargetTypeGateway || engine.Spec.Target.Name == "" {
		a.RevokeAndNotify(resourceName)
		return nil
	}

	var gateway gatewayv1.Gateway
	if err := a.reader.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: engine.Spec.Target.Name}, &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			// A deleted Gateway can no longer back an authorized stream.
			a.RevokeAndNotify(resourceName)
			return nil
		}
		return a.refreshFailed(resourceName, fmt.Errorf("get target Gateway %s/%s: %w", engine.Namespace, engine.Spec.Target.Name, err))
	}
	if !gateway.DeletionTimestamp.IsZero() {
		a.RevokeAndNotify(resourceName)
		return nil
	}

	var pods corev1.PodList
	if err := a.reader.List(ctx, &pods,
		client.InNamespace(gateway.Namespace),
		client.MatchingLabels{GatewayNameLabel: gateway.Name},
	); err != nil {
		return a.refreshFailed(resourceName, fmt.Errorf("list Gateway pods: %w", err))
	}

	identities := make(map[string]string, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !authorizedGatewayPod(pod) {
			continue
		}
		account := pod.Spec.ServiceAccountName
		if account == "" {
			continue
		}
		key := namespacedNameKey(pod.Namespace, account)
		if _, ok := identities[key]; ok {
			continue
		}
		var serviceAccount corev1.ServiceAccount
		if err := a.serviceAccounts.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: account}, &serviceAccount); err != nil {
			if apierrors.IsNotFound(err) {
				// The pod is terminating or orphaned; it must not keep access.
				continue
			}
			return a.refreshFailed(resourceName, fmt.Errorf("get ServiceAccount %s: %w", key, err))
		}
		identities[key] = string(serviceAccount.UID)
	}

	a.mu.Lock()
	if a.revocations[resourceName] != generation {
		// The resource was revoked while this refresh was reading state; the
		// revocation wins and the next event re-refreshes.
		a.mu.Unlock()
		a.Logger.V(1).Info("authorization refresh superseded by a revocation", "resource", resourceName)
		return nil
	}
	previous, existed := a.entries[resourceName]
	revoked := existed && identitiesRevoked(previous.Identities, identities)
	a.entries[resourceName] = AuthzEntry{
		ResourceName: resourceName,
		EngineUID:    engine.UID,
		Namespace:    engine.Namespace,
		EngineName:   engine.Name,
		GatewayName:  gateway.Name,
		Identities:   identities,
		RefreshedAt:  a.now(),
	}
	a.mu.Unlock()

	// Rebinding a Gateway or removing a workload ServiceAccount must end live
	// streams immediately, not on the next revalidation tick.
	if revoked {
		a.revoke(resourceName)
	}
	return nil
}

// authorizedGatewayPod reports whether pod may contribute its
// ServiceAccount to the authorized identity set. The gateway-name label is
// self-assertable, so a pod must also be Running, not terminating, and
// controlled by a ReplicaSet/Deployment owner chain - the shape of every
// Gateway data plane this operator supports. A pod whose ownership cannot
// be established fails closed.
//
// Remaining trust boundary: a principal allowed to create Pods in the
// Gateway's namespace can still mimic a managed pod, because labels and
// owner references are client-settable at creation and are not validated
// against the Gateway's own workloads. Closing that needs a controller the
// operator does not have; until then, pod-create permission in the
// namespace must be treated as equivalent to "may read this Engine's
// rules".
func authorizedGatewayPod(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	if !pod.DeletionTimestamp.IsZero() {
		return false
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return false
	}
	return owner.APIVersion == "apps/v1" && (owner.Kind == "ReplicaSet" || owner.Kind == "Deployment")
}

// identitiesRevoked reports whether a previously authorized ServiceAccount is
// no longer part of the mapping, or was recreated with a new UID.
func identitiesRevoked(previous, next map[string]string) bool {
	for key, uid := range previous {
		nextUID, ok := next[key]
		if !ok {
			return true
		}
		if uid != "" && nextUID != "" && uid != nextUID {
			return true
		}
	}
	return false
}

// RefreshAll re-reads every Engine this index has seen. A deleted Engine is
// revoked. It is the periodic staleness bound for authorization.
//
// Each refresh runs against the revocation generations captured when the pass
// snapshotted its keys, so a revocation issued mid-pass (by this pass or a
// concurrent reconcile) cannot be undone by a later iteration of the same
// pass.
func (a *AuthzIndex) RefreshAll(ctx context.Context) {
	a.mu.RLock()
	keys := make(map[string]types.NamespacedName, len(a.known))
	generations := make(map[string]uint64, len(a.known))
	for resourceName, key := range a.known {
		keys[resourceName] = key
		generations[resourceName] = a.revocations[resourceName]
	}
	a.mu.RUnlock()

	for resourceName, key := range keys {
		var engine wafv1alpha1.Engine
		if err := a.reader.Get(ctx, key, &engine); err != nil {
			if apierrors.IsNotFound(err) {
				a.RevokeAndNotify(resourceName)
				a.mu.Lock()
				delete(a.known, resourceName)
				a.mu.Unlock()
			} else {
				a.logRefreshFailure(resourceName, fmt.Errorf("get Engine %s: %w", key, err))
			}
			continue
		}
		// A delete-and-recreate can coalesce into one event, so the old
		// UID-based resource name is never revoked explicitly. Re-authorizing
		// it with the new Engine would keep the stale name (and its streams)
		// alive forever, so revoke it and let the new name re-register.
		if ResourceName(engine.Namespace, engine.Name, engine.UID) != resourceName {
			a.RevokeAndNotify(resourceName)
			a.mu.Lock()
			delete(a.known, resourceName)
			a.mu.Unlock()
			continue
		}
		if err := a.refresh(ctx, &engine, resourceName, generations[resourceName]); err != nil {
			a.logRefreshFailure(resourceName, err)
		}
	}
}

// logRefreshFailure reports a failed mapping refresh from the periodic sweep.
// V(1) keeps a persistently broken mapping from flooding the log on every
// tick; the staleness bound still revokes it.
func (a *AuthzIndex) logRefreshFailure(resourceName string, err error) {
	a.Logger.V(1).Info("authorization refresh failed", "resource", resourceName, "error", err.Error())
}

// Authorize reports whether id may subscribe to resourceName. Unknown,
// expired, and unauthorized resources all return false.
func (a *AuthzIndex) Authorize(resourceName string, id Identity) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	entry, ok := a.entries[resourceName]
	if !ok {
		return false
	}
	if a.now().Sub(entry.RefreshedAt) > a.staleness {
		return false
	}
	serviceAccountUID, ok := entry.Identities[id.Key()]
	if !ok {
		return false
	}
	if serviceAccountUID != "" && id.UID != "" && serviceAccountUID != id.UID {
		return false
	}
	return true
}

// RevokeAndNotify removes an entry and closes its live streams immediately.
func (a *AuthzIndex) RevokeAndNotify(resourceName string) {
	a.mu.Lock()
	_, existed := a.entries[resourceName]
	delete(a.entries, resourceName)
	a.revocations[resourceName]++
	a.mu.Unlock()
	if existed {
		a.revoke(resourceName)
	}
}

// RevokeEngine revokes every resource belonging to an Engine, including a
// stale entry left under a previous UID. It also forgets the Engine so
// RefreshAll does not resurrect it.
func (a *AuthzIndex) RevokeEngine(namespace, engineName string) {
	a.mu.Lock()
	affected := make(map[string]struct{})
	var revoked []string
	for resourceName, entry := range a.entries {
		if entry.Namespace == namespace && entry.EngineName == engineName {
			affected[resourceName] = struct{}{}
			revoked = append(revoked, resourceName)
		}
	}
	for resourceName, key := range a.known {
		if key.Namespace == namespace && key.Name == engineName {
			affected[resourceName] = struct{}{}
		}
	}
	for resourceName := range affected {
		delete(a.entries, resourceName)
		delete(a.known, resourceName)
		a.revocations[resourceName]++
	}
	a.mu.Unlock()

	for _, resourceName := range revoked {
		a.revoke(resourceName)
	}
}

// refreshFailed keeps the last good mapping until it is older than the
// staleness bound, then revokes it so no stream outlives a stale mapping.
func (a *AuthzIndex) refreshFailed(resourceName string, err error) error {
	a.mu.Lock()
	entry, ok := a.entries[resourceName]
	stale := ok && a.now().Sub(entry.RefreshedAt) > a.staleness
	if stale {
		delete(a.entries, resourceName)
		a.revocations[resourceName]++
	}
	a.mu.Unlock()

	if stale {
		a.revoke(resourceName)
	}
	return err
}

// EngineKey returns the stable "<namespace>/<engine>" snapshot key for an
// authorized resource.
func (a *AuthzIndex) EngineKey(resourceName string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	entry, ok := a.entries[resourceName]
	if !ok {
		return "", false
	}
	return namespacedNameKey(entry.Namespace, entry.EngineName), true
}

func engineAcceptedForGeneration(engine *wafv1alpha1.Engine) bool {
	if engine == nil || engine.Status == nil {
		return false
	}
	condition := apimeta.FindStatusCondition(engine.Status.Conditions, conditionAccepted)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return false
	}
	if condition.ObservedGeneration != engine.Generation {
		return false
	}
	switch condition.Reason {
	case "TargetConflict", "TargetNotFound":
		return false
	}
	return true
}
