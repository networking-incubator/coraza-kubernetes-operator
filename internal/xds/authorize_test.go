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
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

const (
	testNamespace   = "shop"
	testEngineName  = "checkout"
	testGatewayName = "gw"
	testSAKey       = "shop/gateway"
)

func authorizationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := wafv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

type authorizationFixture struct {
	engine  *wafv1alpha1.Engine
	gateway *gatewayv1.Gateway
	pod     *corev1.Pod
	account *corev1.ServiceAccount
}

// gatewayPodOwner is the controller owner reference of a managed Gateway
// data-plane pod.
func gatewayPodOwner() []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       "gw-rs",
		UID:        types.UID("rs-uid"),
		Controller: &controller,
	}}
}

func newAuthorizationFixture() authorizationFixture {
	engine := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  testNamespace,
			Name:       testEngineName,
			UID:        types.UID("engine-uid"),
			Generation: 1,
		},
		Spec: wafv1alpha1.EngineSpec{
			Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: testGatewayName,
			},
			RuleSet: wafv1alpha1.RuleSetReference{Name: "rules"},
		},
		Status: &wafv1alpha1.EngineStatus{
			Conditions: []metav1.Condition{{
				Type:               conditionAccepted,
				Status:             metav1.ConditionTrue,
				Reason:             "Accepted",
				ObservedGeneration: 1,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testGatewayName},
	}

	account := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "gateway", UID: types.UID("sa-uid")},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            "gateway-pod",
			Labels:          map[string]string{GatewayNameLabel: testGatewayName},
			OwnerReferences: gatewayPodOwner(),
		},
		Spec:   corev1.PodSpec{ServiceAccountName: "gateway"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	return authorizationFixture{engine: engine, gateway: gateway, pod: pod, account: account}
}

func newAuthorizationClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(authorizationScheme(t)).WithObjects(objects...).Build()
}

func TestGatewayPodSelector(t *testing.T) {
	selector := GatewayPodSelector()
	if !selector.Matches(labels.Set{GatewayNameLabel: "gw"}) {
		t.Fatal("selector does not match a pod carrying the gateway-name label")
	}
	if selector.Matches(labels.Set{"app": "gw"}) {
		t.Fatal("selector matched a pod without the gateway-name label")
	}
}

func TestGatewayPodTransform(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:     testNamespace,
			Name:          "gateway-pod",
			Labels:        map[string]string{GatewayNameLabel: testGatewayName},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "test"}},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "gateway"},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			PodIP:             "10.0.0.1",
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "gateway"}},
		},
	}

	transformed, err := GatewayPodTransform(pod)
	if err != nil {
		t.Fatalf("GatewayPodTransform() error = %v", err)
	}
	trimmed, ok := transformed.(*corev1.Pod)
	if !ok {
		t.Fatalf("GatewayPodTransform() returned %T, want *corev1.Pod", transformed)
	}
	if trimmed.Status.Phase != corev1.PodRunning {
		t.Fatal("transform dropped PodStatus.Phase, which pod authorization needs")
	}
	if trimmed.Status.PodIP != "" || trimmed.Status.Conditions != nil || trimmed.Status.ContainerStatuses != nil {
		t.Fatal("transform kept churny PodStatus fields")
	}
	if trimmed.ManagedFields != nil {
		t.Fatal("transform kept managed fields; a per-object Transform replaces DefaultTransform")
	}
	if trimmed.Spec.ServiceAccountName != "gateway" || trimmed.Labels[GatewayNameLabel] != testGatewayName {
		t.Fatal("transform dropped fields the mappers read")
	}
	if pod.Status.PodIP == "" || pod.ManagedFields == nil {
		t.Fatal("transform mutated the informer's object instead of a copy")
	}

	other, err := GatewayPodTransform(&corev1.ServiceAccount{})
	if err != nil {
		t.Fatalf("GatewayPodTransform(non-pod) error = %v", err)
	}
	if _, ok := other.(*corev1.ServiceAccount); !ok {
		t.Fatal("GatewayPodTransform() must pass non-pods through unchanged")
	}
}

type recordingRevoker struct {
	mu    sync.Mutex
	names map[string]struct{}
}

func newRecordingRevoker() *recordingRevoker {
	return &recordingRevoker{names: make(map[string]struct{})}
}

func (r *recordingRevoker) RevokeResource(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names[name] = struct{}{}
}

func (r *recordingRevoker) revoked(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.names[name]
	return ok
}

func TestAuthzIndexAuthorizesOnlyGatewayServiceAccounts(t *testing.T) {
	fixture := newAuthorizationFixture()
	index := NewAuthzIndex(newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account), nil, time.Hour, nil)
	ctx := context.Background()

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
	granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}
	if !index.Authorize(resource, granted) {
		t.Fatal("Authorize(gateway SA) = false, want true")
	}
	if index.Authorize(resource, Identity{Namespace: testNamespace, Name: "other", UID: "sa-uid"}) {
		t.Fatal("Authorize(other SA) = true, want false")
	}
	if index.Authorize(ResourceName(testNamespace, "other-engine", "uid"), granted) {
		t.Fatal("Authorize(unknown resource) = true, want false")
	}
	if index.Authorize(resource, Identity{Namespace: testNamespace, Name: "gateway", UID: "stale-uid"}) {
		t.Fatal("Authorize(stale SA UID) = true, want false")
	}
}

// TestAuthzIndexIgnoresSpoofedGatewayPods proves that a pod carrying the
// gateway-name label does not get its ServiceAccount authorized unless it
// also looks like a managed, Running Gateway data-plane pod.
func TestAuthzIndexIgnoresSpoofedGatewayPods(t *testing.T) {
	deletionTimestamp := metav1.Now()
	controller := true
	spoofs := map[string]func(*corev1.Pod){
		"pending phase": func(pod *corev1.Pod) {
			pod.Status.Phase = corev1.PodPending
		},
		"succeeded phase": func(pod *corev1.Pod) {
			pod.Status.Phase = corev1.PodSucceeded
		},
		"terminating": func(pod *corev1.Pod) {
			// The fake client refuses a deletionTimestamp without finalizers.
			pod.Finalizers = []string{"test.coraza.io/keep"}
			pod.DeletionTimestamp = &deletionTimestamp
		},
		"no controller owner": func(pod *corev1.Pod) {
			pod.OwnerReferences = nil
		},
		"job-owned": func(pod *corev1.Pod) {
			pod.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       "attacker",
				UID:        types.UID("job-uid"),
				Controller: &controller,
			}}
		},
		"legacy-apiowner": func(pod *corev1.Pod) {
			pod.OwnerReferences[0].APIVersion = "extensions/v1beta1"
		},
	}

	for name, spoof := range spoofs {
		t.Run(name, func(t *testing.T) {
			fixture := newAuthorizationFixture()
			spoof(fixture.pod)
			index := NewAuthzIndex(newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account), nil, time.Hour, nil)

			if err := index.Refresh(context.Background(), fixture.engine); err != nil {
				t.Fatalf("Refresh() error = %v", err)
			}
			resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
			granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}
			if index.Authorize(resource, granted) {
				t.Fatalf("Authorize() = true for a spoofed pod (%s)", name)
			}
		})
	}

	// The unspoofed fixture stays authorized.
	fixture := newAuthorizationFixture()
	index := NewAuthzIndex(newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account), nil, time.Hour, nil)
	if err := index.Refresh(context.Background(), fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
	if !index.Authorize(resource, Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}) {
		t.Fatal("Authorize() = false for a managed Running Gateway pod")
	}
}

func TestAuthzIndexRevokesDeletedPodAndEngine(t *testing.T) {
	fixture := newAuthorizationFixture()
	fakeClient := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account)
	revoker := newRecordingRevoker()
	index := NewAuthzIndex(fakeClient, nil, time.Hour, revoker.RevokeResource)
	ctx := context.Background()
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
	granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if !index.Authorize(resource, granted) {
		t.Fatal("Authorize() = false before pod deletion")
	}

	if err := fakeClient.Delete(ctx, fixture.pod); err != nil {
		t.Fatal(err)
	}
	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if index.Authorize(resource, granted) {
		t.Fatal("Authorize() = true after the only Gateway pod was deleted")
	}

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	index.RevokeEngine(testNamespace, testEngineName)
	if !revoker.revoked(resource) {
		t.Fatal("RevokeEngine() did not revoke the live resource")
	}
	if index.Authorize(resource, granted) {
		t.Fatal("Authorize() = true after RevokeEngine()")
	}
}

func TestAuthzIndexRevokesUnacceptedEngine(t *testing.T) {
	fixture := newAuthorizationFixture()

	revoker := newRecordingRevoker()
	index := NewAuthzIndex(newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account), nil, time.Hour, revoker.RevokeResource)
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)

	if err := index.Refresh(context.Background(), fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if !index.Authorize(resource, Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}) {
		t.Fatal("Authorize() = false for an accepted Engine")
	}

	fixture.engine.Status.Conditions[0].Status = metav1.ConditionFalse
	fixture.engine.Status.Conditions[0].Reason = "TargetConflict"
	if err := index.Refresh(context.Background(), fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if index.Authorize(resource, Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}) {
		t.Fatal("Authorize() = true for an Engine with Accepted=False")
	}
	if !revoker.revoked(resource) {
		t.Fatal("Refresh() did not revoke an unaccepted Engine")
	}
}

func TestAuthzIndexRevokesDeletedGateway(t *testing.T) {
	fixture := newAuthorizationFixture()
	base := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account)
	revoker := newRecordingRevoker()
	index := NewAuthzIndex(base, nil, time.Hour, revoker.RevokeResource)
	ctx := context.Background()
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
	granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatal(err)
	}
	if !index.Authorize(resource, granted) {
		t.Fatal("Authorize() = false before Gateway deletion")
	}

	if err := base.Delete(ctx, fixture.gateway); err != nil {
		t.Fatal(err)
	}
	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if index.Authorize(resource, granted) {
		t.Fatal("Authorize() = true after the Gateway was deleted")
	}
	if !revoker.revoked(resource) {
		t.Fatal("Gateway deletion did not revoke the resource")
	}
}

func TestAuthzIndexRebindingRevokesLiveStreams(t *testing.T) {
	fixture := newAuthorizationFixture()
	newAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "gateway-v2", UID: types.UID("sa-uid-v2")},
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       testNamespace,
			Name:            "gateway-pod-v2",
			Labels:          map[string]string{GatewayNameLabel: testGatewayName},
			OwnerReferences: gatewayPodOwner(),
		},
		Spec:   corev1.PodSpec{ServiceAccountName: "gateway-v2"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	fakeClient := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account, newAccount, newPod)
	revoker := newRecordingRevoker()
	index := NewAuthzIndex(fakeClient, nil, time.Hour, revoker.RevokeResource)
	ctx := context.Background()
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatal(err)
	}
	if err := fakeClient.Delete(ctx, fixture.pod); err != nil {
		t.Fatal(err)
	}
	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	if !revoker.revoked(resource) {
		t.Fatal("removing an authorized ServiceAccount did not revoke live streams")
	}
	if index.Authorize(resource, Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}) {
		t.Fatal("authorization for the removed ServiceAccount survived")
	}
	if !index.Authorize(resource, Identity{Namespace: testNamespace, Name: "gateway-v2", UID: "sa-uid-v2"}) {
		t.Fatal("authorization for the new ServiceAccount is missing")
	}
}

func TestAuthzIndexStalenessRejectsOldMapping(t *testing.T) {
	fixture := newAuthorizationFixture()
	index := NewAuthzIndex(newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account), nil, time.Minute, nil)
	now := time.Unix(1_000_000, 0)
	index.now = func() time.Time { return now }
	ctx := context.Background()
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
	granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatal(err)
	}
	if !index.Authorize(resource, granted) {
		t.Fatal("Authorize() = false while fresh")
	}
	now = now.Add(2 * time.Minute)
	if index.Authorize(resource, granted) {
		t.Fatal("Authorize() = true for a mapping older than the staleness bound")
	}
}

// gatingReader blocks the first matching Get after Arm, so a test can act
// while the index is between its evidence snapshot and its write.
type gatingReader struct {
	client.Reader
	entered chan struct{}
	release chan struct{}
	match   func(client.Object) bool
}

// Arm returns the channels of the next matching Get: entered is closed when
// that Get arrives, and closing release lets it proceed.
func (r *gatingReader) Arm() (entered, release chan struct{}) {
	r.entered = make(chan struct{})
	r.release = make(chan struct{})
	return r.entered, r.release
}

func (r *gatingReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if r.entered != nil && r.match(object) {
		entered := r.entered
		r.entered = nil
		close(entered)
		<-r.release
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

// TestAuthzIndexRefreshAllDoesNotResurrectRevocation proves the reported race:
// RefreshAll snapshots its keys, an explicit RevokeEngine lands mid-pass, and
// the stale iteration must not re-insert what the revoke deleted.
func TestAuthzIndexRefreshAllDoesNotResurrectRevocation(t *testing.T) {
	fixture := newAuthorizationFixture()
	base := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account)
	gated := &gatingReader{
		Reader:  base,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		match:   func(object client.Object) bool { _, ok := object.(*wafv1alpha1.Engine); return ok },
	}
	revoker := newRecordingRevoker()
	index := NewAuthzIndex(gated, base, time.Hour, revoker.RevokeResource)
	ctx := context.Background()
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
	granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if !index.Authorize(resource, granted) {
		t.Fatal("Authorize() = false before the race")
	}

	done := make(chan struct{})
	entered, release := gated.Arm()
	go func() {
		index.RefreshAll(ctx)
		close(done)
	}()
	<-entered // the pass has snapshotted its keys and is reading the Engine

	index.RevokeEngine(testNamespace, testEngineName)
	if !revoker.revoked(resource) {
		t.Fatal("RevokeEngine() did not revoke the live resource")
	}

	close(release) // the stale iteration resumes with the Engine still readable
	<-done

	if index.Authorize(resource, granted) {
		t.Fatal("a stale RefreshAll pass resurrected a revoked Engine")
	}
	if _, ok := index.EngineKey(resource); ok {
		t.Fatal("a stale RefreshAll pass re-added a revoked Engine to the known set")
	}
}

// TestAuthzIndexRefreshLosesToConcurrentRevocation proves the mid-refresh
// race: a revoke issued while a refresh is reading state wins over that
// refresh's write, and a refresh with fresh evidence re-authorizes.
func TestAuthzIndexRefreshLosesToConcurrentRevocation(t *testing.T) {
	fixture := newAuthorizationFixture()
	base := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account)
	gated := &gatingReader{
		Reader: base,
		match:  func(object client.Object) bool { _, ok := object.(*corev1.ServiceAccount); return ok },
	}
	index := NewAuthzIndex(base, gated, time.Hour, newRecordingRevoker().RevokeResource)
	ctx := context.Background()
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
	granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	done := make(chan error, 1)
	entered, release := gated.Arm()
	go func() { done <- index.Refresh(ctx, fixture.engine) }()
	<-entered // the refresh is between its reads and its write

	index.RevokeEngine(testNamespace, testEngineName)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if index.Authorize(resource, granted) {
		t.Fatal("a refresh lost the race against an explicit revocation")
	}

	// A refresh with fresh evidence re-authorizes: an Engine that is
	// re-accepted must not stay locked out.
	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if !index.Authorize(resource, granted) {
		t.Fatal("Authorize() = false for a fresh refresh of a live Engine")
	}
}

type erroringReader struct {
	client.Reader
	failList bool
	failGet  bool
}

func (r erroringReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if r.failGet {
		return errors.New("api server unavailable")
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

func (r erroringReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if r.failList {
		return errors.New("api server unavailable")
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestAuthzIndexStalenessBoundOnRefreshFailure(t *testing.T) {
	fixture := newAuthorizationFixture()
	base := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account)
	reader := &erroringReader{Reader: base}
	revoker := newRecordingRevoker()
	index := NewAuthzIndex(reader, nil, time.Minute, revoker.RevokeResource)
	now := time.Unix(1_000_000, 0)
	index.now = func() time.Time { return now }
	ctx := context.Background()
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
	granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatal(err)
	}
	reader.failList = true
	if err := index.Refresh(ctx, fixture.engine); err == nil {
		t.Fatal("Refresh() error = nil, want failure")
	}
	if !index.Authorize(resource, granted) {
		t.Fatal("Authorize() = false while the last good mapping is still fresh")
	}

	now = now.Add(2 * time.Minute)
	if err := index.Refresh(ctx, fixture.engine); err == nil {
		t.Fatal("Refresh() error = nil, want failure")
	}
	if index.Authorize(resource, granted) {
		t.Fatal("Authorize() = true after the mapping exceeded the staleness bound")
	}
	if !revoker.revoked(resource) {
		t.Fatal("stale mapping was not revoked")
	}
}

func TestAuthzIndexRefreshAllRemovesDeletedEngine(t *testing.T) {
	fixture := newAuthorizationFixture()
	base := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account)
	revoker := newRecordingRevoker()
	index := NewAuthzIndex(base, base, time.Hour, revoker.RevokeResource)
	ctx := context.Background()
	resource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)
	granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if !index.Authorize(resource, granted) {
		t.Fatal("Authorize() = false before deleting the Engine")
	}
	if err := base.Delete(ctx, fixture.engine); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	index.RefreshAll(ctx)

	if !revoker.revoked(resource) {
		t.Fatal("RefreshAll() did not revoke the deleted Engine")
	}
	if _, ok := index.EngineKey(resource); ok {
		t.Fatal("RefreshAll() kept the deleted Engine's entry")
	}
	index.mu.RLock()
	_, known := index.known[resource]
	_, entry := index.entries[resource]
	index.mu.RUnlock()
	if known {
		t.Fatal("RefreshAll() kept the deleted Engine in the known set")
	}
	if entry {
		t.Fatal("RefreshAll() kept the deleted Engine's authorization entry")
	}
}

func TestEngineAcceptedForGeneration(t *testing.T) {
	fixture := newAuthorizationFixture()
	if !engineAcceptedForGeneration(fixture.engine) {
		t.Fatal("engineAcceptedForGeneration() = false for an accepted Engine")
	}
	fixture.engine.Generation = 2
	if engineAcceptedForGeneration(fixture.engine) {
		t.Fatal("engineAcceptedForGeneration() = true for a stale Accepted condition")
	}
	apimeta.SetStatusCondition(&fixture.engine.Status.Conditions, metav1.Condition{
		Type: conditionAccepted, Status: metav1.ConditionTrue, Reason: "TargetNotFound", ObservedGeneration: 2,
	})
	if engineAcceptedForGeneration(fixture.engine) {
		t.Fatal("engineAcceptedForGeneration() = true for TargetNotFound")
	}
}

// An Engine delete-and-recreate can coalesce into one event, so the old
// UID-based resource name is never revoked explicitly. RefreshAll must revoke
// it instead of re-authorizing the stale name with the new Engine.
func TestAuthzIndexRefreshAllRevokesRecreatedEngineName(t *testing.T) {
	fixture := newAuthorizationFixture()
	base := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account)
	revoker := newRecordingRevoker()
	index := NewAuthzIndex(base, base, time.Hour, revoker.RevokeResource)
	ctx := context.Background()
	granted := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}
	oldResource := ResourceName(testNamespace, testEngineName, fixture.engine.UID)

	if err := index.Refresh(ctx, fixture.engine); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if !index.Authorize(oldResource, granted) {
		t.Fatal("Authorize() = false before the recreate")
	}

	recreated := fixture.engine.DeepCopy()
	recreated.UID = types.UID("engine-uid-2")
	recreated.ResourceVersion = ""
	if err := base.Delete(ctx, fixture.engine); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := base.Create(ctx, recreated); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	index.RefreshAll(ctx)

	if index.Authorize(oldResource, granted) {
		t.Fatal("RefreshAll() kept the previous UID-based resource name authorized")
	}
	if _, ok := index.EngineKey(oldResource); ok {
		t.Fatal("RefreshAll() kept the previous UID-based resource name in the known set")
	}
	if !revoker.revoked(oldResource) {
		t.Fatal("RefreshAll() did not revoke the previous UID-based resource name")
	}
}
