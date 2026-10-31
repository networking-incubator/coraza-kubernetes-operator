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
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		// No ServiceAccount watch: authorization resolves ServiceAccounts
		// through the direct API reader (no cluster-wide SA informer), so a
		// rotated ServiceAccount is caught by the next refresh event or the
		// authorization staleness bound at the latest.
		WithOptions(controller.Options{
			NeedLeaderElection:      &notLeader,
			MaxConcurrentReconciles: 2,
			RateLimiter:             publicationRateLimiter(),
		}).
		Named("publication").
		Complete(r)
}

// publicationRateLimiter pairs the per-item exponential backoff with the
// controller-runtime default overall token bucket (10 qps, 100 burst). Without
// the bucket, a burst of successfully-reconciling items — the Pod and
// ServiceAccount watches fan out into this controller — is unthrottled.
func publicationRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter[reconcile.Request](
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](1*time.Second, 1*time.Minute),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
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
	// is refreshed on every reconcile, including Gateway-only events. A failed
	// refresh is returned so the workqueue retries; otherwise a Pod that just
	// became Running could stay unauthorized until the periodic sweep.
	refreshErr := r.Authz.Refresh(ctx, &engine)
	if refreshErr != nil {
		log.V(1).Info("Publication: authorization refresh failed", "error", refreshErr.Error())
	}

	accepted := acceptedConfiguration(&engine)
	if accepted == nil {
		// ClearSnapshot orphans the proxy's open watch, so a withdrawn
		// configuration must also revoke the streams: without revocation the
		// proxy keeps enforcing stale rules and a later re-accept is not
		// delivered until the stream reconnects on its own.
		r.forgetEngine(req)
		return ctrl.Result{}, nil
	}

	if err := r.reconstruct(ctx, req.NamespacedName, &engine, accepted); err != nil {
		var transient *transientError
		if errors.As(err, &transient) {
			return ctrl.Result{}, transient.err
		}

		// Keep the previous snapshot and wait for the next event. The readiness
		// deadline reports each stuck cold record exactly once. Only a
		// divergence from the accepted record is a hash mismatch; unavailable
		// inputs, assemble failures, and limit violations are not.
		if errors.Is(err, render.ErrAcceptedMismatch) {
			r.Metrics.HashMismatch()
		}
		r.Readiness.RecordFailure(engine.Namespace, engine.Name, err)
		log.V(1).Info("Publication: configuration does not match the accepted record", "reason", err.Error())
		r.recordMismatch(req.NamespacedName)
		return ctrl.Result{}, refreshErr
	}

	r.Readiness.RecordSuccess(engine.Namespace, engine.Name)
	r.clearMismatch(req.NamespacedName)
	return ctrl.Result{}, refreshErr
}

func (r *PublicationReconciler) reconstruct(ctx context.Context, key types.NamespacedName, engine *wafv1alpha1.Engine, accepted *wafv1alpha1.EngineAcceptedConfiguration) error {
	limits := r.Limits.WithDefaults()

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
	if r.coalesced(key, fingerprint, accepted.Version) {
		return nil
	}

	content, err := assemble.Assemble(inputs)
	if err != nil {
		return fmt.Errorf("assemble RuleSet content: %w", err)
	}
	if err := validateContentLimits(content, limits); err != nil {
		return err
	}

	vmSettings, err := acceptedRenderingInputs(accepted)
	if err != nil {
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
		EmptyRulesPolicy:      string(accepted.EmptyRulesPolicy),
		FailurePolicy:         string(accepted.FailurePolicy),
		EnableFilterStateLogs: accepted.FilterStateLogs == wafv1alpha1.FilterStateLogsEnabled,
		DriverType:            string(accepted.DriverType),
		Binary: render.BinaryRef{
			URL:    accepted.BinaryURL,
			SHA256: accepted.WasmSHA256,
		},
		VMSettings: vmSettings,
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

	if err := validateRenderedPayload(artifact, limits); err != nil {
		return err
	}

	if err := r.Store.Replace(&xds.Snapshot{
		ResourceName: artifact.ResourceName,
		Namespace:    engine.Namespace,
		EngineName:   engine.Name,
		EngineUID:    engine.UID,
		RuleSetHash:  accepted.RuleSetContentHash,
		Version:      artifact.Version,
		Resource:     artifact.Resource,
	}); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("ECDS configuration published",
		"version", artifact.Version,
		"ruleSources", len(inputs.Sources),
		"ruleBytes", len(content.Rules),
		"dataFiles", len(content.DataFiles),
	)
	r.Metrics.SetSnapshotVersion(engine.Namespace, engine.Name)
	r.remember(key, fingerprint, accepted.Version)
	return nil
}

// acceptedRenderingInputs validates that the accepted record carries every
// resolved rendering input and returns its VM settings. A record written
// before these inputs were persisted must not be rendered with recomputed
// defaults: it mismatches and waits for re-acceptance instead of publishing a
// configuration the leader never accepted.
func acceptedRenderingInputs(accepted *wafv1alpha1.EngineAcceptedConfiguration) (render.VMSettings, error) {
	empty := accepted.EmptyRulesPolicy == "" ||
		accepted.FilterStateLogs == "" ||
		accepted.DriverType == "" ||
		accepted.VM.Runtime == "" ||
		accepted.VM.VMID == "" ||
		accepted.VM.CodeTimeout == ""
	if empty {
		return render.VMSettings{}, fmt.Errorf("accepted configuration does not record all rendering inputs")
	}
	codeTimeout, err := time.ParseDuration(accepted.VM.CodeTimeout)
	if err != nil {
		return render.VMSettings{}, fmt.Errorf("accepted VM code timeout %q is not a duration: %w", accepted.VM.CodeTimeout, err)
	}
	return render.VMSettings{
		Runtime:     accepted.VM.Runtime,
		VMID:        accepted.VM.VMID,
		CodeTimeout: codeTimeout,
	}, nil
}

func (r *PublicationReconciler) coalesced(key types.NamespacedName, fingerprint, acceptedVersion string) bool {
	r.mu.Lock()
	state, ok := r.states[key]
	r.mu.Unlock()
	if !ok || state.fingerprint != fingerprint || state.acceptedVersion != acceptedVersion {
		return false
	}

	// The inputs and accepted record are unchanged. Skip rendering only when
	// the store already holds the accepted version.
	if snapshot, exists := r.Store.Get(key.String()); exists {
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

// clearMismatch keeps the coalescing fingerprint after a successful publish,
// clearing only the lag marker. clearState is for an Engine that no longer has
// an accepted record.
func (r *PublicationReconciler) clearMismatch(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state, ok := r.states[key]; ok {
		state.mismatchSince = time.Time{}
	}
	r.updatePublicationLagLocked()
}

func (r *PublicationReconciler) clearState(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.states, key)
	r.updatePublicationLagLocked()
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
	r.updatePublicationLagLocked()
}

// updatePublicationLagLocked reports the oldest outstanding mismatch, not the
// last Engine reconciled. Caller holds r.mu.
func (r *PublicationReconciler) updatePublicationLagLocked() {
	var oldest time.Duration
	for _, state := range r.states {
		if state.mismatchSince.IsZero() {
			continue
		}
		if lag := time.Since(state.mismatchSince); lag > oldest {
			oldest = lag
		}
	}
	r.Metrics.SetPublicationLag(oldest.Seconds())
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

// messageEnvelopeBytes reserves the DiscoveryResponse wrapper the xDS server
// adds around a rendered resource. It lives in the xds package because the
// startup limits check needs the same reservation.
const messageEnvelopeBytes = xds.MessageEnvelopeBytes

func validateContentLimits(content assemble.Content, limits xds.Limits) error {
	if len(content.Rules) > limits.MaxMessageBytes {
		return fmt.Errorf("rule text size %d exceeds the message limit of %d", len(content.Rules), limits.MaxMessageBytes)
	}
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

func validateRenderedPayload(artifact render.Artifact, limits xds.Limits) error {
	if size := proto.Size(artifact.Resource) + messageEnvelopeBytes; size > limits.MaxMessageBytes {
		return fmt.Errorf("ECDS response size %d exceeds the message limit of %d", size, limits.MaxMessageBytes)
	}
	// envoy-coraza rejects a plugin configuration above its own cap, so a
	// payload that only fits the gRPC message would still be NACKed.
	if len(artifact.Payload) > render.MaxConfigurationBytes {
		return fmt.Errorf("ECDS payload %d exceeds the plugin configuration limit of %d", len(artifact.Payload), render.MaxConfigurationBytes)
	}
	// The runtime payload's schema_version is render.SchemaVersion by
	// construction and pinned by render tests; re-decoding the multi-megabyte
	// payload here bought nothing. The accepted record's rendering schema is
	// verified against the artifact by render.Matches before this runs.
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
			return acceptedRecordKey(oldEngine) != acceptedRecordKey(newEngine) ||
				acceptedConditionKey(oldEngine) != acceptedConditionKey(newEngine)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// acceptedConditionKey captures the condition publish authorization depends on.
// A condition-only update does not change the Engine generation, but can revoke
// or restore an ECDS stream.
func acceptedConditionKey(engine *wafv1alpha1.Engine) string {
	if engine == nil || engine.Status == nil {
		return ""
	}
	condition := apimeta.FindStatusCondition(engine.Status.Conditions, conditionAccepted)
	if condition == nil {
		return ""
	}
	return fmt.Sprintf("%s|%s|%d", condition.Status, condition.Reason, condition.ObservedGeneration)
}

func acceptedRecordKey(engine *wafv1alpha1.Engine) string {
	accepted := acceptedConfiguration(engine)
	if accepted == nil {
		return ""
	}
	// Every persisted field participates: a record rewritten by a newer
	// operator (for example completed with the rendering inputs) must fire
	// the publication watch even when the accepted version is unchanged.
	return fmt.Sprintf("%d|%d|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s",
		accepted.ObservedGeneration,
		accepted.RenderingSchemaVersion,
		accepted.Version,
		accepted.RuleSetUID,
		accepted.RuleSetContentHash,
		accepted.BinaryURL,
		accepted.WasmSHA256,
		accepted.EmptyRulesPolicy,
		accepted.FilterStateLogs,
		accepted.DriverType,
		accepted.VM.Runtime,
		accepted.VM.VMID,
		accepted.VM.CodeTimeout,
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
			if !hasGatewayLabel(e.ObjectOld) && !hasGatewayLabel(e.ObjectNew) {
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
			// Pods become authorized identities when they reach Running and
			// stop being authorized when they succeed, fail, or are deleted.
			// Neither transition changes the labels, so phase must be part of
			// the comparison; otherwise revocation is delayed until the next
			// refresh tick. Keep these fields in sync with authorizedGatewayPod.
			return oldPod.Labels[gatewayNameLabel] != newPod.Labels[gatewayNameLabel] ||
				oldPod.Spec.ServiceAccountName != newPod.Spec.ServiceAccountName ||
				oldPod.Status.Phase != newPod.Status.Phase ||
				oldPod.DeletionTimestamp.IsZero() != newPod.DeletionTimestamp.IsZero() ||
				!apiequality.Semantic.DeepEqual(metav1.GetControllerOf(oldPod), metav1.GetControllerOf(newPod))
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
