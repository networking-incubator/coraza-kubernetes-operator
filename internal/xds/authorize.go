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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

const (
	gatewayNameLabel  = "gateway.networking.k8s.io/gateway-name"
	conditionAccepted = "Accepted"
)

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

// Revoker is notified when a resource must be revoked immediately. The
// StreamRegistry implements it.
type Revoker interface {
	RevokeResource(resourceName string)
}

// AuthzIndex maps Engine resource names to the ServiceAccounts of the pods
// backing the Engine's target Gateway.
type AuthzIndex struct {
	reader    client.Reader
	staleness time.Duration
	now       func() time.Time

	mu      sync.RWMutex
	entries map[string]AuthzEntry
	known   map[string]types.NamespacedName

	revoker Revoker
}

// NewAuthzIndex returns an empty index. reader should be the manager's cached
// client so mapping refreshes do not hammer the API server.
func NewAuthzIndex(reader client.Reader, staleness time.Duration) *AuthzIndex {
	if staleness <= 0 {
		staleness = DefaultAuthorizationMaxStaleness
	}
	return &AuthzIndex{
		reader:    reader,
		staleness: staleness,
		now:       time.Now,
		entries:   make(map[string]AuthzEntry),
		known:     make(map[string]types.NamespacedName),
	}
}

// SetRevoker wires immediate stream revocation.
func (a *AuthzIndex) SetRevoker(revoker Revoker) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.revoker = revoker
}

// Refresh recomputes the mapping for one Engine. An Engine that is deleting,
// not accepted for its current generation, or not targeting a live Gateway is
// revoked. A transient API failure keeps the previous mapping until it exceeds
// the staleness bound, then revokes it.
func (a *AuthzIndex) Refresh(ctx context.Context, engine *wafv1alpha1.Engine) error {
	if engine == nil {
		return nil
	}
	resourceName := ResourceName(engine.Namespace, engine.Name, engine.UID)
	a.mu.Lock()
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
		client.MatchingLabels{gatewayNameLabel: gateway.Name},
	); err != nil {
		return a.refreshFailed(resourceName, fmt.Errorf("list Gateway pods: %w", err))
	}

	identities := make(map[string]string, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		account := pod.Spec.ServiceAccountName
		if account == "" {
			continue
		}
		key := pod.Namespace + "/" + account
		if _, ok := identities[key]; ok {
			continue
		}
		var serviceAccount corev1.ServiceAccount
		if err := a.reader.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: account}, &serviceAccount); err != nil {
			if apierrors.IsNotFound(err) {
				// The pod is terminating or orphaned; it must not keep access.
				continue
			}
			return a.refreshFailed(resourceName, fmt.Errorf("get ServiceAccount %s: %w", key, err))
		}
		identities[key] = string(serviceAccount.UID)
	}

	a.mu.Lock()
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
	revoker := a.revoker
	a.mu.Unlock()

	// Rebinding a Gateway or removing a workload ServiceAccount must end live
	// streams immediately, not on the next revalidation tick.
	if revoked && revoker != nil {
		revoker.RevokeResource(resourceName)
	}
	return nil
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
func (a *AuthzIndex) RefreshAll(ctx context.Context) {
	a.mu.RLock()
	keys := make(map[string]types.NamespacedName, len(a.known))
	for resourceName, key := range a.known {
		keys[resourceName] = key
	}
	a.mu.RUnlock()

	for resourceName, key := range keys {
		var engine wafv1alpha1.Engine
		if err := a.reader.Get(ctx, key, &engine); err != nil {
			if apierrors.IsNotFound(err) {
				a.RevokeAndNotify(resourceName)
			} else {
				_ = a.refreshFailed(resourceName, fmt.Errorf("get Engine %s: %w", key, err))
			}
			continue
		}
		_ = a.Refresh(ctx, &engine)
	}
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
	revoker := a.revoker
	a.mu.Unlock()
	if existed && revoker != nil {
		revoker.RevokeResource(resourceName)
	}
}

// RevokeEngine revokes every resource belonging to an Engine, including a
// stale entry left under a previous UID. It also forgets the Engine so
// RefreshAll does not resurrect it.
func (a *AuthzIndex) RevokeEngine(namespace, engineName string) {
	a.mu.Lock()
	var revoked []string
	for resourceName, entry := range a.entries {
		if entry.Namespace == namespace && entry.EngineName == engineName {
			delete(a.entries, resourceName)
			revoked = append(revoked, resourceName)
		}
	}
	for resourceName, key := range a.known {
		if key.Namespace == namespace && key.Name == engineName {
			delete(a.known, resourceName)
		}
	}
	revoker := a.revoker
	a.mu.Unlock()

	if revoker == nil {
		return
	}
	for _, resourceName := range revoked {
		revoker.RevokeResource(resourceName)
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
	}
	revoker := a.revoker
	a.mu.Unlock()

	if stale && revoker != nil {
		revoker.RevokeResource(resourceName)
	}
	return err
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
