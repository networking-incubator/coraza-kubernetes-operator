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
			Namespace: testNamespace,
			Name:      "gateway-pod",
			Labels:    map[string]string{gatewayNameLabel: testGatewayName},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "gateway"},
	}

	return authorizationFixture{engine: engine, gateway: gateway, pod: pod, account: account}
}

func newAuthorizationClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(authorizationScheme(t)).WithObjects(objects...).Build()
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
	index := NewAuthzIndex(newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account), time.Hour)
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

func TestAuthzIndexRevokesDeletedPodAndEngine(t *testing.T) {
	fixture := newAuthorizationFixture()
	fakeClient := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account)
	index := NewAuthzIndex(fakeClient, time.Hour)
	revoker := newRecordingRevoker()
	index.SetRevoker(revoker)
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

	index := NewAuthzIndex(newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account), time.Hour)
	revoker := newRecordingRevoker()
	index.SetRevoker(revoker)
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
	index := NewAuthzIndex(base, time.Hour)
	revoker := newRecordingRevoker()
	index.SetRevoker(revoker)
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
			Namespace: testNamespace,
			Name:      "gateway-pod-v2",
			Labels:    map[string]string{gatewayNameLabel: testGatewayName},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "gateway-v2"},
	}
	fakeClient := newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account, newAccount, newPod)
	index := NewAuthzIndex(fakeClient, time.Hour)
	revoker := newRecordingRevoker()
	index.SetRevoker(revoker)
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
	index := NewAuthzIndex(newAuthorizationClient(t, fixture.engine, fixture.gateway, fixture.pod, fixture.account), time.Minute)
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
	index := NewAuthzIndex(reader, time.Minute)
	revoker := newRecordingRevoker()
	index.SetRevoker(revoker)
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
