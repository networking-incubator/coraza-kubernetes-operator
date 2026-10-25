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
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/xds"
)

// PublicationReconciler publishes the accepted Engine configuration on every
// replica. It never writes status, never renders desired state, and keeps the
// previous snapshot whenever its local reconstruction does not reproduce the
// accepted record byte-for-byte.
type PublicationReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme

	Store     *xds.SnapshotStore
	Authz     *xds.AuthzIndex
	Readiness *xds.PublicationReadiness
	Metrics   *xds.Metrics
	Limits    xds.Limits

	mu     sync.Mutex
	states map[types.NamespacedName]*publicationState
}

type publicationState struct {
	fingerprint     string
	acceptedVersion string
	mismatchSince   time.Time
}

// SetupWithManager registers the publication controller on every replica.
func (r *PublicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	notLeader := false

	return ctrl.NewControllerManagedBy(mgr).
		For(&wafv1alpha1.Engine{}, builder.WithPredicates(enginePublicationPredicate())).
		Watches(&wafv1alpha1.RuleSet{}, handler.EnqueueRequestsFromMapFunc(r.mapRuleSet),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&wafv1alpha1.RuleSource{}, handler.EnqueueRequestsFromMapFunc(r.mapRuleInput),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&wafv1alpha1.RuleData{}, handler.EnqueueRequestsFromMapFunc(r.mapRuleInput),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&gatewayv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.mapGateway),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPod),
			builder.WithPredicates(podAuthorizationPredicate())).
		Watches(&corev1.ServiceAccount{}, handler.EnqueueRequestsFromMapFunc(r.mapServiceAccount),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{
			NeedLeaderElection:      &notLeader,
			MaxConcurrentReconciles: 2,
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
				1*time.Second, 1*time.Minute),
		}).
		Named("publication").
		Complete(r)
}

// Reconcile reconstructs and publishes one accepted Engine configuration.
func (r *PublicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var engine wafv1alpha1.Engine
	if err := r.Get(ctx, req.NamespacedName, &engine); err != nil {
		if apierrors.IsNotFound(err) {
			r.forgetEngine(req)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !engine.DeletionTimestamp.IsZero() {
		r.forgetEngine(req)
		return ctrl.Result{}, nil
	}

	// Authorization depends only on Engine/Gateway/Pod/ServiceAccount state and
	// is refreshed on every reconcile, including Gateway-only events.
	if err := r.Authz.Refresh(ctx, &engine); err != nil {
		log.V(1).Info("Publication: authorization refresh failed", "error", err.Error())
	}

	accepted := acceptedConfiguration(&engine)
	if accepted == nil {
		r.Store.DeleteEngine(engine.Namespace, engine.Name)
		r.Metrics.ForgetSnapshot(engine.Namespace, engine.Name)
		r.Readiness.RecordSuccess(engine.Namespace, engine.Name)
		r.clearState(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	if err := r.reconstruct(ctx, req.NamespacedName, &engine, accepted); err != nil {
		var transient *transientError
		if errors.As(err, &transient) {
			return ctrl.Result{}, transient.err
		}

		// Keep the previous snapshot and wait for the next event.
		r.Metrics.HashMismatch()
		if r.Readiness.RecordFailure(engine.Namespace, engine.Name, err) {
			r.Metrics.ColdReconstructionFailure()
		}
		log.V(1).Info("Publication: configuration does not match the accepted record", "reason", err.Error())
		r.recordMismatch(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	r.Metrics.SetPublicationLag(0)
	r.Readiness.RecordSuccess(engine.Namespace, engine.Name)
	r.clearState(req.NamespacedName)
	return ctrl.Result{}, nil
}

func (r *PublicationReconciler) reconstruct(ctx context.Context, key types.NamespacedName, engine *wafv1alpha1.Engine, accepted *wafv1alpha1.EngineAcceptedConfiguration) error {
	limits := r.Limits
	if limits.MaxMessageBytes <= 0 {
		limits = xds.DefaultLimits()
	}

	var ruleset wafv1alpha1.RuleSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: engine.Spec.RuleSet.Name}, &ruleset); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("referenced RuleSet is not available")
		}
		return &transientError{err: err}
	}
	if string(ruleset.UID) != accepted.RuleSetUID {
		return fmt.Errorf("RuleSet UID does not match the accepted record")
	}

	inputs, err := loadRuleSetInputs(ctx, r.Client, &ruleset)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("a referenced RuleSet input is not available")
		}
		return &transientError{err: err}
	}

	fingerprint := assemble.Fingerprint(inputs)
	resourceName := xds.ResourceName(engine.Namespace, engine.Name, engine.UID)
	if r.coalesced(key, fingerprint, accepted.Version, resourceName) {
		return nil
	}

	content, err := assemble.Assemble(inputs)
	if err != nil {
		return fmt.Errorf("assemble RuleSet content: %w", err)
	}
	if err := validateContentLimits(content, limits); err != nil {
		return err
	}

	artifact, err := render.Render(render.EngineInput{
		Namespace: engine.Namespace,
		Name:      engine.Name,
		UID:       engine.UID,
		RuleSet: render.RuleSetAccepted{
			UID:         ruleset.UID,
			ContentHash: accepted.RuleSetContentHash,
		},
		Content:               content,
		EmptyRulesPolicy:      string(ruleset.Spec.EmptyRulesPolicy),
		FailurePolicy:         string(accepted.FailurePolicy),
		EnableFilterStateLogs: engine.Spec.Observability.Mode == wafv1alpha1.ObservabilityModeEnabled,
		DriverType:            string(engine.Spec.Driver.Type),
		Binary: render.BinaryRef{
			URL:    accepted.BinaryURL,
			SHA256: accepted.WasmSHA256,
		},
	})
	if err != nil {
		return fmt.Errorf("render accepted configuration: %w", err)
	}

	if err := render.Matches(render.AcceptedEngine{
		RenderingSchemaVersion: accepted.RenderingSchemaVersion,
		Version:                accepted.Version,
		RuleSetUID:             accepted.RuleSetUID,
		RuleSetContentHash:     accepted.RuleSetContentHash,
		BinaryURL:              accepted.BinaryURL,
		WasmSHA256:             accepted.WasmSHA256,
		FailurePolicy:          string(accepted.FailurePolicy),
	}, artifact); err != nil {
		return err
	}

	if err := validateRenderedPayload(artifact.Payload, limits); err != nil {
		return err
	}

	r.Store.Replace(&xds.Snapshot{
		ResourceName: artifact.ResourceName,
		Namespace:    engine.Namespace,
		EngineName:   engine.Name,
		EngineUID:    engine.UID,
		RuleSetHash:  accepted.RuleSetContentHash,
		Version:      artifact.Version,
		Resource:     artifact.Resource,
	})
	r.Metrics.SetSnapshotVersion(engine.Namespace, engine.Name)
	r.remember(key, fingerprint, accepted.Version)
	return nil
}

func (r *PublicationReconciler) coalesced(key types.NamespacedName, fingerprint, acceptedVersion, resourceName string) bool {
	r.mu.Lock()
	state, ok := r.states[key]
	r.mu.Unlock()
	if !ok || state.fingerprint != fingerprint || state.acceptedVersion != acceptedVersion {
		return false
	}

	// The inputs and accepted record are unchanged. Skip rendering only when
	// the store already holds the accepted version.
	if snapshot, exists := r.Store.Get(resourceName); exists {
		return snapshot.Version == acceptedVersion
	}
	return false
}

func (r *PublicationReconciler) remember(key types.NamespacedName, fingerprint, acceptedVersion string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.states == nil {
		r.states = make(map[types.NamespacedName]*publicationState)
	}
	r.states[key] = &publicationState{fingerprint: fingerprint, acceptedVersion: acceptedVersion}
}

func (r *PublicationReconciler) clearState(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.states, key)
}

func (r *PublicationReconciler) recordMismatch(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.states == nil {
		r.states = make(map[types.NamespacedName]*publicationState)
	}
	state, ok := r.states[key]
	if !ok {
		state = &publicationState{}
		r.states[key] = state
	}
	if state.mismatchSince.IsZero() {
		state.mismatchSince = time.Now()
	}
	r.Metrics.SetPublicationLag(time.Since(state.mismatchSince).Seconds())
}

func (r *PublicationReconciler) forgetEngine(req ctrl.Request) {
	r.Store.DeleteEngine(req.Namespace, req.Name)
	r.Authz.RevokeEngine(req.Namespace, req.Name)
	r.Metrics.ForgetSnapshot(req.Namespace, req.Name)
	r.Readiness.RecordSuccess(req.Namespace, req.Name)
	r.clearState(req.NamespacedName)
}

type transientError struct {
	err error
}

func (e *transientError) Error() string {
	return e.err.Error()
}

func (e *transientError) Unwrap() error {
	return e.err
}

// acceptedConfiguration returns the accepted record, or nil when the Engine
// has not been accepted yet.
func acceptedConfiguration(engine *wafv1alpha1.Engine) *wafv1alpha1.EngineAcceptedConfiguration {
	if engine == nil || engine.Status == nil {
		return nil
	}
	return engine.Status.AcceptedConfiguration
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

func validateContentLimits(content assemble.Content, limits xds.Limits) error {
	if len(content.DataFiles) > limits.MaxDataFiles {
		return fmt.Errorf("RuleData file count %d exceeds the limit of %d", len(content.DataFiles), limits.MaxDataFiles)
	}
	total := 0
	for _, data := range content.DataFiles {
		total += len(data)
	}
	if total > limits.MaxDecodedDataBytes {
		return fmt.Errorf("decoded RuleData size %d exceeds the limit of %d", total, limits.MaxDecodedDataBytes)
	}
	return nil
}

func validateRenderedPayload(payload []byte, limits xds.Limits) error {
	if len(payload) > limits.MaxMessageBytes {
		return fmt.Errorf("runtime payload size %d exceeds the message limit of %d", len(payload), limits.MaxMessageBytes)
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(payload, &header); err != nil {
		return fmt.Errorf("decode runtime payload: %w", err)
	}
	if header.SchemaVersion != render.SchemaVersion {
		return fmt.Errorf("runtime payload schema version %d is not supported", header.SchemaVersion)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Predicates
// -----------------------------------------------------------------------------

func enginePublicationPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldEngine, ok := e.ObjectOld.(*wafv1alpha1.Engine)
			if !ok {
				return false
			}
			newEngine, ok := e.ObjectNew.(*wafv1alpha1.Engine)
			if !ok {
				return false
			}
			if oldEngine.Generation != newEngine.Generation {
				return true
			}
			return acceptedRecordKey(oldEngine) != acceptedRecordKey(newEngine)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func acceptedRecordKey(engine *wafv1alpha1.Engine) string {
	accepted := acceptedConfiguration(engine)
	if accepted == nil {
		return ""
	}
	return fmt.Sprintf("%d|%d|%s|%s|%s|%s|%s",
		accepted.ObservedGeneration,
		accepted.RenderingSchemaVersion,
		accepted.Version,
		accepted.RuleSetUID,
		accepted.RuleSetContentHash,
		accepted.BinaryURL,
		accepted.WasmSHA256,
	)
}

func podAuthorizationPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return hasGatewayLabel(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return hasGatewayLabel(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			oldPod, ok := e.ObjectOld.(*corev1.Pod)
			if !ok {
				return false
			}
			newPod, ok := e.ObjectNew.(*corev1.Pod)
			if !ok {
				return false
			}
			return oldPod.Labels[gatewayNameLabel] != newPod.Labels[gatewayNameLabel] ||
				oldPod.Spec.ServiceAccountName != newPod.Spec.ServiceAccountName ||
				oldPod.DeletionTimestamp.IsZero() != newPod.DeletionTimestamp.IsZero()
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func hasGatewayLabel(object client.Object) bool {
	if object == nil {
		return false
	}
	_, ok := object.GetLabels()[gatewayNameLabel]
	return ok
}

// -----------------------------------------------------------------------------
// Mapping
// -----------------------------------------------------------------------------

func (r *PublicationReconciler) mapRuleSet(ctx context.Context, object client.Object) []reconcile.Request {
	var engines wafv1alpha1.EngineList
	if err := r.List(ctx, &engines, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(engines.Items))
	for i := range engines.Items {
		engine := &engines.Items[i]
		if engine.Spec.RuleSet.Name == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: engine.Namespace, Name: engine.Name}})
		}
	}
	return requests
}

func (r *PublicationReconciler) mapRuleInput(ctx context.Context, object client.Object) []reconcile.Request {
	var ruleSets wafv1alpha1.RuleSetList
	if err := r.List(ctx, &ruleSets, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}

	var requests []reconcile.Request
	seen := make(map[types.NamespacedName]struct{})
	for i := range ruleSets.Items {
		ruleSet := &ruleSets.Items[i]
		if !ruleSetReferences(ruleSet, object) {
			continue
		}
		for _, request := range r.mapRuleSet(ctx, ruleSet) {
			if _, ok := seen[request.NamespacedName]; ok {
				continue
			}
			seen[request.NamespacedName] = struct{}{}
			requests = append(requests, request)
		}
	}
	return requests
}

func ruleSetReferences(ruleSet *wafv1alpha1.RuleSet, object client.Object) bool {
	switch object.(type) {
	case *wafv1alpha1.RuleSource:
		for _, source := range ruleSet.Spec.Sources {
			if source.Name == object.GetName() {
				return true
			}
		}
	case *wafv1alpha1.RuleData:
		for _, data := range ruleSet.Spec.Data {
			if data.Name == object.GetName() {
				return true
			}
		}
	}
	return false
}

func (r *PublicationReconciler) mapGateway(ctx context.Context, object client.Object) []reconcile.Request {
	return r.enginesTargetingGateway(ctx, object.GetNamespace(), object.GetName())
}

func (r *PublicationReconciler) mapPod(ctx context.Context, object client.Object) []reconcile.Request {
	gateway, ok := object.GetLabels()[gatewayNameLabel]
	if !ok {
		return nil
	}
	return r.enginesTargetingGateway(ctx, object.GetNamespace(), gateway)
}

func (r *PublicationReconciler) mapServiceAccount(ctx context.Context, object client.Object) []reconcile.Request {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}

	var requests []reconcile.Request
	seen := make(map[types.NamespacedName]struct{})
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.ServiceAccountName != object.GetName() {
			continue
		}
		gateway, ok := pod.Labels[gatewayNameLabel]
		if !ok {
			continue
		}
		for _, request := range r.enginesTargetingGateway(ctx, pod.Namespace, gateway) {
			if _, ok := seen[request.NamespacedName]; ok {
				continue
			}
			seen[request.NamespacedName] = struct{}{}
			requests = append(requests, request)
		}
	}
	return requests
}

func (r *PublicationReconciler) enginesTargetingGateway(ctx context.Context, namespace, gateway string) []reconcile.Request {
	var engines wafv1alpha1.EngineList
	if err := r.List(ctx, &engines, client.InNamespace(namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(engines.Items))
	for i := range engines.Items {
		engine := &engines.Items[i]
		if engine.Spec.Target.Type == wafv1alpha1.EngineTargetTypeGateway && engine.Spec.Target.Name == gateway {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: engine.Namespace, Name: engine.Name}})
		}
	}
	return requests
}
