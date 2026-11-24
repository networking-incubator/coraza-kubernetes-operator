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
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	wasmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	"google.golang.org/protobuf/types/known/anypb"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

const (
	validToken   = "valid-token"
	otherToken   = "other-token"
	streamEngine = "checkout"
)

// serverStack is the xDS server with a fake TokenReview backend that accepts
// validToken for identity, and podToken(uid) for identity bound to pod uid,
// and an authorization entry for resourceName backed by testGatewayPods pods.
type serverStack struct {
	server        *Server
	store         *SnapshotStore
	authz         *AuthzIndex
	streams       *StreamRegistry
	metrics       *Metrics
	fakeClient    *k8sfake.Clientset
	resourceName  string
	identity      Identity
	setTokenValid func(bool)
	setTokenDelay func(time.Duration)
}

func newServerStack(t *testing.T, limits Limits) *serverStack {
	t.Helper()

	identity := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}
	resourceName := ResourceName(testNamespace, streamEngine, types.UID("engine-uid"))

	var tokenMu sync.Mutex
	tokenValid := true
	var tokenDelay time.Duration
	fakeClient := k8sfake.NewSimpleClientset()
	fakeClient.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		request := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		tokenMu.Lock()
		valid := tokenValid
		delay := tokenDelay
		tokenMu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		return true, reviewFor(identity, request.Spec.Token, valid), nil
	})

	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}

	store := NewSnapshotStore()
	var streams *StreamRegistry
	authz := NewAuthzIndex(nil, nil, time.Hour, func(resourceName string) {
		streams.RevokeResource(resourceName)
	})
	authz.entries[resourceName] = AuthzEntry{
		ResourceName: resourceName,
		EngineUID:    types.UID("engine-uid"),
		Namespace:    testNamespace,
		EngineName:   streamEngine,
		GatewayName:  testGatewayName,
		Identities:   map[string]string{identity.Key(): identity.UID},
		Pods:         map[string]int{identity.Key(): testGatewayPods},
		RefreshedAt:  time.Now(),
	}

	auth := authzTokenReviewer(fakeClient)
	streams = NewStreamRegistry(auth, authz, limits, metrics)

	return &serverStack{
		server:       NewServer(store, auth, authz, streams, limits, metrics),
		store:        store,
		authz:        authz,
		streams:      streams,
		metrics:      metrics,
		fakeClient:   fakeClient,
		resourceName: resourceName,
		identity:     identity,
		setTokenValid: func(valid bool) {
			tokenMu.Lock()
			tokenValid = valid
			tokenMu.Unlock()
		},
		setTokenDelay: func(delay time.Duration) {
			tokenMu.Lock()
			tokenDelay = delay
			tokenMu.Unlock()
		},
	}
}

// testGatewayPods is how many authorized pods run as the fixture identity.
const testGatewayPods = 16

// podToken is a test token the fake TokenReview binds to pod uid.
func podToken(uid string) string { return "pod:" + uid }

// reviewFor answers a TokenReview the way the API server does for the
// fixture's tokens.
func reviewFor(identity Identity, token string, valid bool) *authv1.TokenReview {
	podUID, podBound := strings.CutPrefix(token, "pod:")
	if !valid || (token != validToken && !podBound) {
		return rejectedReview()
	}
	review := &authv1.TokenReview{Status: authv1.TokenReviewStatus{
		Authenticated: true,
		User:          authv1.UserInfo{Username: "system:serviceaccount:" + identity.Namespace + ":" + identity.Name, UID: identity.UID},
	}}
	if podBound {
		review.Status.User.Extra = map[string]authv1.ExtraValue{podUIDExtraKey: {podUID}}
	}
	return review
}

// streamFixture is a gRPC client of the production TLS listener.
type streamFixture struct {
	*listenerFixture
	client  extensionv3.ExtensionConfigDiscoveryServiceClient
	proxyID string
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	return newStreamFixtureWithLimits(t, limits)
}

func newStreamFixtureWithLimits(t *testing.T, limits Limits) *streamFixture {
	t.Helper()
	listener := newListenerFixtureWithLimits(t, limits)
	return &streamFixture{listenerFixture: listener, client: listener.newECDSClient(t), proxyID: "test-proxy"}
}

// newECDSClient opens a separate connection, the way each Gateway pod has its own.
func (f *listenerFixture) newECDSClient(t *testing.T) extensionv3.ExtensionConfigDiscoveryServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(f.listener.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: f.pool, ServerName: "localhost"})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return extensionv3.NewExtensionConfigDiscoveryServiceClient(conn)
}

func authzTokenReviewer(client *k8sfake.Clientset) *Authenticator {
	return NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Hour, time.Second, 0)
}

func (f *streamFixture) open(t *testing.T, token string, names ...string) extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient {
	t.Helper()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	stream, err := f.client.StreamExtensionConfigs(ctx)
	if err != nil {
		t.Fatalf("StreamExtensionConfigs() error = %v", err)
	}
	send(t, stream, &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: f.proxyID}, ResourceNames: names})
	return stream
}

// send sends req. A refused stream may already be closed; io.EOF then defers
// the status to the next Recv, as gRPC specifies.
func send(t *testing.T, stream extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient, req *discoveryv3.DiscoveryRequest) {
	t.Helper()
	if err := stream.Send(req); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Send() error = %v", err)
	}
}

func (f *streamFixture) openAs(t *testing.T, proxyID string, names ...string) extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient {
	t.Helper()
	previous := f.proxyID
	f.proxyID = proxyID
	defer func() { f.proxyID = previous }()
	return f.open(t, validToken, names...)
}

// wire is the xDS version of a snapshot version sent under this process's tag
// of generation gen.
func (f *streamFixture) wire(version string, gen uint64) string {
	return wireVersion("sha256:"+version, tag{epoch: f.store.epoch, gen: gen})
}

// node is the cache node of the Engine's tag snapshot of generation gen.
func (f *streamFixture) node(gen uint64) string {
	return tagNode(testNamespace+"/"+streamEngine, tag{epoch: f.store.epoch, gen: gen})
}

// vmID is the VM ID the Engine's snapshot carries under the tag of generation gen.
func (f *streamFixture) vmID(gen uint64) string {
	return "coraza-engine-uid." + tag{epoch: f.store.epoch, gen: gen}.String()
}

// generation is the generation of an Engine's current tag.
func (f *streamFixture) generation(engineKey string) uint64 {
	f.store.mu.RLock()
	defer f.store.mu.RUnlock()
	return f.store.engines[engineKey].gen
}

func responseVMID(t *testing.T, response *discoveryv3.DiscoveryResponse) string {
	t.Helper()
	require.Len(t, response.GetResources(), 1)
	var typed corev3.TypedExtensionConfig
	require.NoError(t, response.Resources[0].UnmarshalTo(&typed))
	var wasm wasmhttp.Wasm
	require.NoError(t, typed.TypedConfig.UnmarshalTo(&wasm))
	return wasm.GetConfig().GetVmConfig().GetVmId()
}

func (f *streamFixture) snapshot(t *testing.T, version string) *Snapshot {
	t.Helper()
	typed, err := anypb.New(&wasmhttp.Wasm{Config: &wasmv3.PluginConfig{
		Vm: &wasmv3.PluginConfig_VmConfig{VmConfig: &wasmv3.VmConfig{
			VmId: "coraza-engine-uid", NackOnCodeCacheMiss: true,
		}},
	}})
	require.NoError(t, err)
	return &Snapshot{
		ResourceName: f.resourceName,
		Namespace:    testNamespace,
		EngineName:   streamEngine,
		EngineUID:    types.UID("engine-uid"),
		RuleSetHash:  "sha256:" + version,
		Version:      "sha256:" + version,
		Resource:     &corev3.TypedExtensionConfig{Name: f.resourceName, TypedConfig: typed},
	}
}

func (f *streamFixture) snapshotFor(t *testing.T, namespace, engine string, uid types.UID, version string) *Snapshot {
	t.Helper()
	snapshot := f.snapshot(t, version)
	snapshot.Namespace = namespace
	snapshot.EngineName = engine
	snapshot.EngineUID = uid
	snapshot.ResourceName = ResourceName(namespace, engine, uid)
	snapshot.Resource.Name = snapshot.ResourceName
	return snapshot
}

func recv(t *testing.T, stream extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient) *discoveryv3.DiscoveryResponse {
	t.Helper()
	response, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	return response
}

func recvCode(t *testing.T, stream extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient) codes.Code {
	t.Helper()
	_, err := stream.Recv()
	if err == nil {
		t.Fatal("Recv() error = nil, want failure")
	}
	return status.Code(err)
}

func TestStreamServesOneResourceWithMonotonicNonce(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	// The initial request carries a spoofed node.id; the server must pin the
	// watch to the authorized Engine, not the client-supplied identity.
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken)
	stream, err := fixture.client.StreamExtensionConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "spoofed"},
		ResourceNames: []string{fixture.resourceName},
	}); err != nil {
		t.Fatal(err)
	}
	first := recv(t, stream)
	if first.GetVersionInfo() != fixture.wire("v1", 0) {
		t.Fatalf("version_info = %q, want %q", first.GetVersionInfo(), fixture.wire("v1", 0))
	}
	if first.GetNonce() != "1" {
		t.Fatalf("nonce = %q, want 1", first.GetNonce())
	}
	if first.GetTypeUrl() != resource.ExtensionConfigType || len(first.GetResources()) != 1 {
		t.Fatalf("response = %+v", first)
	}

	// A client-supplied node.id must not repoint the cache watch on ACK.
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "spoofed"},
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: first.GetNonce(),
	}); err != nil {
		t.Fatal(err)
	}

	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	second := recv(t, stream)
	if second.GetVersionInfo() != fixture.wire("v2", 0) || second.GetNonce() != "2" {
		t.Fatalf("second response = version %q nonce %q", second.GetVersionInfo(), second.GetNonce())
	}
}

func TestECDSUnaryAndDeltaAreUnimplemented(t *testing.T) {
	fixture := newStreamFixture(t)
	victim := fixture.snapshotFor(t, "victim-ns", "victim-engine", types.UID("victim-uid"), "victim-v1")
	require.NoError(t, fixture.store.Replace(victim))

	// No token is supplied: these RPCs must not reach the snapshot cache at all.
	_, err := fixture.client.FetchExtensionConfigs(context.Background(), &discoveryv3.DiscoveryRequest{
		Node:    &corev3.Node{Id: "victim-ns/victim-engine"},
		TypeUrl: resource.ExtensionConfigType,
	})
	if code := status.Code(err); code != codes.Unimplemented {
		t.Fatalf("FetchExtensionConfigs code = %v, want Unimplemented (err=%v)", code, err)
	}

	delta, err := fixture.client.DeltaExtensionConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delta.Recv(); status.Code(err) != codes.Unimplemented {
		t.Fatalf("DeltaExtensionConfigs code = %v, want Unimplemented (err=%v)", status.Code(err), err)
	}
}

func TestStreamNackAfterAckUsesRejectedVersion(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: first.GetNonce(),
	}); err != nil {
		t.Fatal(err)
	}

	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	second := recv(t, stream)
	if second.GetVersionInfo() != fixture.wire("v2", 0) {
		t.Fatalf("version_info = %q, want %q", second.GetVersionInfo(), fixture.wire("v2", 0))
	}

	// Envoy's NACK carries the previous ACCEPTED version, not the rejected one:
	// the response nonce names the tag it rejected.
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: second.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	require.Eventually(t, func() bool { return fixture.generation(testNamespace+"/"+streamEngine) == 1 }, time.Second, 5*time.Millisecond)
}

func TestStreamNackRepublishesUnderNewVersion(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	nack := func(stream extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient, resp *discoveryv3.DiscoveryResponse) {
		t.Helper()
		// The proxy has accepted nothing yet, and a rejection carries the last
		// accepted version, not the rejected one.
		if err := stream.Send(&discoveryv3.DiscoveryRequest{
			ResourceNames: []string{fixture.resourceName},
			ResponseNonce: resp.GetNonce(),
			ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	engine := testNamespace + "/" + streamEngine

	// A cold proxy rejects the configuration because the remote module is not
	// cached. Envoy never builds bytes it rejected, so the next generation gives
	// the proxy other bytes, and the stream is closed for a resubscribe.
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	if got := first.GetVersionInfo(); got != fixture.wire("v1", 0) {
		t.Fatalf("version_info = %q, want %q", got, fixture.wire("v1", 0))
	}
	nack(stream, first)
	_ = recvCode(t, stream)
	require.Eventually(t, func() bool { return fixture.generation(engine) == 1 }, time.Second, 5*time.Millisecond)
	stream = fixture.open(t, validToken, fixture.resourceName)
	second := recv(t, stream)
	if got := second.GetVersionInfo(); got != fixture.wire("v1", 1) {
		t.Fatalf("resubscribed version = %q, want %q", got, fixture.wire("v1", 1))
	}

	// A second rejection advances the generation again, and the accepted
	// content version never changes so the publication loop still coalesces.
	nack(stream, second)
	_ = recvCode(t, stream)
	require.Eventually(t, func() bool { return fixture.generation(engine) == 2 }, time.Second, 5*time.Millisecond)

	snapshot, ok := fixture.store.Get(engine)
	require.True(t, ok)
	if snapshot.Version != "sha256:v1" {
		t.Fatalf("accepted version = %q, want sha256:v1", snapshot.Version)
	}
}

// A rejection moves only the proxy that rejected on to the next tag: a proxy
// that accepted the configuration keeps its tag, and is sent nothing.
func TestStreamNackRetryIsScopedToRejectingProxy(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	rejecting := fixture.openAs(t, "proxy-rejecting", fixture.resourceName)
	rejected := recv(t, rejecting)
	acking := fixture.openAs(t, "proxy-acking", fixture.resourceName)
	accepted := recv(t, acking)
	fixture.awaitWritten(t)
	require.Equal(t, fixture.wire("v1", 0), accepted.GetVersionInfo())
	require.Equal(t, fixture.vmID(0), responseVMID(t, accepted))

	require.NoError(t, acking.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   accepted.GetVersionInfo(),
		ResponseNonce: accepted.GetNonce(),
	}))
	require.NoError(t, rejecting.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		ResponseNonce: rejected.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}))
	if code := recvCode(t, rejecting); code != codes.Unavailable {
		t.Fatalf("rejecting stream code = %v, want Unavailable", code)
	}
	require.Eventually(t, func() bool { return fixture.generation(testNamespace+"/"+streamEngine) == 1 }, time.Second, 5*time.Millisecond)

	// The accepted tag's snapshot is kept for the proxy that holds it.
	baseVersion, baseBytes := servedAt(t, fixture.store, fixture.node(0))
	require.Equal(t, fixture.wire("v1", 0), baseVersion)
	require.Equal(t, fixture.vmID(0), vmIDOf(t, baseBytes))

	// Moving the generation on would push a new tag to this already-ACKing watch.
	ackingUpdates := startRecv(acking)
	expectNoResult(t, ackingUpdates)

	retry := fixture.openAs(t, "proxy-rejecting", fixture.resourceName)
	retried := recv(t, retry)
	require.Equal(t, fixture.wire("v1", 1), retried.GetVersionInfo())
	require.Equal(t, fixture.vmID(1), responseVMID(t, retried))
	next, err := fixture.store.cache.GetSnapshot(fixture.node(1))
	require.NoError(t, err, "the resubscribe is served the next generation's snapshot")
	require.Equal(t, fixture.wire("v1", 1), next.GetVersion(resource.ExtensionConfigType))

	// The rejection counts against the rejecting proxy alone.
	require.Equal(t, 1, fixture.record("proxy-rejecting").attempts)
	fixture.awaitAcknowledged(t, "proxy-acking")
	require.Zero(t, fixture.record("proxy-acking").attempts)
	require.NoError(t, retry.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   retried.GetVersionInfo(),
		ResponseNonce: retried.GetNonce(),
	}))
	retryUpdates := startRecv(retry)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	for gen, streamUpdates := range []<-chan recvResult{ackingUpdates, retryUpdates} {
		updated := expectResult(t, streamUpdates)
		require.Equal(t, fixture.wire("v2", uint64(gen)), updated.GetVersionInfo())
		require.Equal(t, fixture.vmID(uint64(gen)), responseVMID(t, updated))
	}
}

func TestStreamNackRetryContinuesAfterManyRejections(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	fixture.awaitWritten(t)
	// A proxy that already rejected configurations many times in a row must keep
	// being retried: giving up strands a fail-closed proxy.
	fixture.seedRejections(t, fixture.proxyID, 100)

	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		ResponseNonce: first.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}

	// The retry continues: the stream is reset and the generation advances for
	// the resubscribe.
	if code := recvCode(t, stream); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
	require.Eventually(t, func() bool { return fixture.generation(testNamespace+"/"+streamEngine) == 1 }, time.Second, 5*time.Millisecond)
	require.Equal(t, 101, fixture.record(fixture.proxyID).attempts)
}

// An acknowledgement ends the proxy's back-off, so a later rejection starts it
// again, and is still retried.
func TestStreamAckEndsTheBackoff(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	// The proxy has already rejected configurations many times in a row.
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	fixture.awaitWritten(t)
	fixture.seedRejections(t, fixture.proxyID, 20)

	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: first.GetNonce(),
	}); err != nil {
		t.Fatal(err)
	}
	require.Eventually(t, func() bool {
		record := fixture.record(fixture.proxyID)
		return record.acked && record.attempts == 0
	}, time.Second, 5*time.Millisecond)

	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(), // accepted by the acknowledgement above
		ResponseNonce: first.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	if code := recvCode(t, stream); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
	require.Equal(t, 1, fixture.record(fixture.proxyID).attempts)
}

func TestStreamNackBeforeSubscriptionStillResubscribes(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	// Envoy may replay the NACK as the first message of a reconnected stream,
	// before it sends a subscription we recorded.
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken)
	stream, err := fixture.client.StreamExtensionConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "test-proxy"},
		ResourceNames: []string{fixture.resourceName},
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	if code := recvCode(t, stream); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
}

func TestStreamNackWithoutResourceNamesStillResubscribes(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	// Envoy may replay the NACK as the first message of a reconnected stream
	// carrying no resource_names: the identity's single authorized resource
	// identifies the rejected configuration.
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken)
	stream, err := fixture.client.StreamExtensionConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		Node:        &corev3.Node{Id: "test-proxy"},
		ErrorDetail: status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	if code := recvCode(t, stream); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
}

func TestStreamRejectsWildcardAndMultipleResources(t *testing.T) {
	fixture := newStreamFixture(t)

	t.Run("wildcard", func(t *testing.T) {
		stream := fixture.open(t, validToken)
		if code := recvCode(t, stream); code != codes.InvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument", code)
		}
	})

	t.Run("multiple", func(t *testing.T) {
		stream := fixture.open(t, validToken, fixture.resourceName, fixture.resourceName+"-other")
		if code := recvCode(t, stream); code != codes.InvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument", code)
		}
	})

	t.Run("unknown format", func(t *testing.T) {
		stream := fixture.open(t, validToken, "not-a-coraza-resource")
		if code := recvCode(t, stream); code != codes.InvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument", code)
		}
	})
}

func TestStreamRequiresAuthenticationAndAuthorization(t *testing.T) {
	fixture := newStreamFixture(t)

	t.Run("missing token", func(t *testing.T) {
		ctx := context.Background()
		stream, err := fixture.client.StreamExtensionConfigs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		send(t, stream, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}})
		if code := recvCode(t, stream); code != codes.Unauthenticated {
			t.Fatalf("code = %v, want Unauthenticated", code)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		stream := fixture.open(t, otherToken, fixture.resourceName)
		if code := recvCode(t, stream); code != codes.Unauthenticated {
			t.Fatalf("code = %v, want Unauthenticated", code)
		}
	})

	t.Run("unknown and unauthorized look the same", func(t *testing.T) {
		unknown := fixture.open(t, validToken, ResourceName(testNamespace, "other", types.UID("other-uid")))
		if code := recvCode(t, unknown); code != codes.Unavailable {
			t.Fatalf("unknown resource code = %v, want Unavailable", code)
		}
	})
}

func TestStreamIgnoresNodeIdentity(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken)
	stream, err := fixture.client.StreamExtensionConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A spoofed node.id must not grant access to another Engine.
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "gateway-pod"},
		ResourceNames: []string{ResourceName(testNamespace, "other", types.UID("other-uid"))},
	}); err != nil {
		t.Fatal(err)
	}
	if code := recvCode(t, stream); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
}

func TestFirstAckCannotWatchAnotherEngine(t *testing.T) {
	fixture := newStreamFixture(t)
	victim := fixture.snapshotFor(t, "victim-ns", "victim-engine", types.UID("victim-uid"), "victim-v1")
	require.NoError(t, fixture.store.Replace(victim))

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken)
	stream, err := fixture.client.StreamExtensionConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// No subscription exists, so a name-less first-message ACK is refused and
	// cannot create a watch on the client-supplied Engine key.
	send(t, stream, &discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "victim-ns/victim-engine"},
		ResponseNonce: "spoofed-nonce",
	})
	if code := recvCode(t, stream); code != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", code)
	}
	require.Zero(t, fixture.watches(tagNode("victim-ns/victim-engine", tag{epoch: fixture.store.epoch})))
}

func TestFirstNackCannotBumpAnotherEngine(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	victim := fixture.snapshotFor(t, "victim-ns", "victim-engine", types.UID("victim-uid"), "victim-v1")
	require.NoError(t, fixture.store.Replace(victim))

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken)
	stream, err := fixture.client.StreamExtensionConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "victim-ns/victim-engine"},
		ResourceNames: []string{victim.ResourceName},
		VersionInfo:   victim.Version,
		ResponseNonce: "spoofed-nonce",
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	if code := recvCode(t, stream); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
	fixture.server.deliveries.mu.Lock()
	records := fixture.server.deliveries.order.Len()
	fixture.server.deliveries.mu.Unlock()
	if records != 0 {
		t.Fatal("an unauthorized NACK must not write a delivery record")
	}
	if got := fixture.generation("victim-ns/victim-engine"); got != 0 {
		t.Fatalf("generation = %d, want 0: an unauthorized NACK must not advance another Engine", got)
	}
}

func TestNackDelayGrowsAndIsCapped(t *testing.T) {
	base := 2 * time.Second
	if got := nackDelay(base, 1); got != base {
		t.Fatalf("nackDelay(attempt 1) = %v, want %v", got, base)
	}
	if got := nackDelay(base, 2); got != 4*time.Second {
		t.Fatalf("nackDelay(attempt 2) = %v, want 4s", got)
	}
	if got := nackDelay(base, 4); got != 16*time.Second {
		t.Fatalf("nackDelay(attempt 4) = %v, want 16s", got)
	}
	for _, attempt := range []int{5, 20, 1000} {
		if got := nackDelay(base, attempt); got != nackRetryMaxDelay {
			t.Fatalf("nackDelay(attempt %d) = %v, want %v", attempt, got, nackRetryMaxDelay)
		}
	}
	// Tests that shrink the base keep running instantly.
	if got := nackDelay(0, 7); got != 0 {
		t.Fatalf("nackDelay(base 0, attempt 7) = %v, want 0", got)
	}
}

// record returns a copy of the delivery record of the proxy with node ID
// proxyID, or nil.
func (f *streamFixture) record(proxyID string) *delivery {
	key := proxyCacheKey(testNamespace+"/"+streamEngine, f.identity, proxyID)
	f.server.deliveries.mu.Lock()
	defer f.server.deliveries.mu.Unlock()
	element, ok := f.server.deliveries.byKey[key]
	if !ok {
		return nil
	}
	record := *element.Value.(*delivery)
	return &record
}

// awaitAcknowledged waits until the proxy with node ID proxyID has acknowledged
// what it was last sent.
func (f *streamFixture) awaitAcknowledged(t *testing.T, proxyID string) {
	t.Helper()
	require.Eventually(t, func() bool { return f.record(proxyID).acked }, 5*time.Second, 5*time.Millisecond)
}

// seedRejections gives the proxy with node ID proxyID, which has been sent a
// configuration, a record of n rejections in a row.
func (f *streamFixture) seedRejections(t *testing.T, proxyID string, n int) {
	t.Helper()
	require.NotNil(t, f.record(proxyID), "a rejection only counts for a proxy that was sent a configuration")
	key := proxyCacheKey(testNamespace+"/"+streamEngine, f.identity, proxyID)
	for range n {
		f.server.deliveries.rejected(key, delivered{})
	}
}

// A proxy's back-off is its own: another proxy's rejections must not stop, or
// lengthen, its retry.
func TestRejectionBackoffIsPerProxy(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	streamA := fixture.openAs(t, "proxy-a", fixture.resourceName)
	_ = recv(t, streamA)
	fixture.seedRejections(t, "proxy-a", 100)

	streamB := fixture.openAs(t, "proxy-b", fixture.resourceName)
	second := recv(t, streamB)
	fixture.awaitWritten(t)
	if err := streamB.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		ResponseNonce: second.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	if code := recvCode(t, streamB); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
	require.Eventually(t, func() bool { return fixture.generation(testNamespace+"/"+streamEngine) == 1 }, time.Second, 5*time.Millisecond)
	require.Equal(t, 1, fixture.record("proxy-b").attempts)
	require.Equal(t, 100, fixture.record("proxy-a").attempts)
}

func TestStreamDoesNotSendAnEmptyResponseOnMiss(t *testing.T) {
	fixture := newStreamFixture(t)
	stream := fixture.open(t, validToken, fixture.resourceName)

	pending := startRecv(stream)
	expectNoResult(t, pending)

	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	response := expectResult(t, pending)
	if response.GetVersionInfo() != fixture.wire("v1", 0) {
		t.Fatalf("version_info = %q, want %q", response.GetVersionInfo(), fixture.wire("v1", 0))
	}
}

func TestStreamRevokedWhenAuthorizationIsRemoved(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	stream := fixture.open(t, validToken, fixture.resourceName)
	_ = recv(t, stream)
	fixture.awaitWritten(t)

	fixture.authz.RevokeAndNotify(fixture.resourceName)
	if code := recvCode(t, stream); code != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", code)
	}
}

func TestStreamUnavailableWhenServerShutsDown(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	stream := fixture.open(t, validToken, fixture.resourceName)
	_ = recv(t, stream)
	fixture.awaitWritten(t)

	fixture.streams.CloseAll()
	if code := recvCode(t, stream); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
}

func TestStreamUnauthenticatedWhenRevalidationFails(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	stream := fixture.open(t, validToken, fixture.resourceName)
	_ = recv(t, stream)
	fixture.awaitWritten(t)

	fixture.setTokenValid(false)
	for range revalidationRejectionsToClose {
		fixture.streams.revalidateAll(context.Background())
	}
	if code := recvCode(t, stream); code != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", code)
	}
}

// TestStreamAuthenticationDeadlineDoesNotCutSlowTokenReview pins the deadline
// to the idle case: a request arriving inside the window stops the timer, so a
// slow TokenReview still completes instead of cancelling the stream.
func TestStreamAuthenticationDeadlineDoesNotCutSlowTokenReview(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	fixture.server.authDeadline = 20 * time.Millisecond
	fixture.setTokenDelay(150 * time.Millisecond)

	stream := fixture.open(t, validToken, fixture.resourceName)
	response := recv(t, stream)
	if response.GetVersionInfo() == "" {
		t.Fatal("slow authentication returned no configuration")
	}
}

// TestStreamIdleDeadlineStillCancels proves the bound still applies to a
// stream that opens and never sends a request.
func TestStreamIdleDeadlineStillCancels(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.authDeadline = 20 * time.Millisecond

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken)
	stream, err := fixture.client.StreamExtensionConfigs(ctx)
	require.NoError(t, err)
	if _, err := stream.Recv(); err == nil {
		t.Fatal("idle stream was not cancelled")
	}

	require.Eventually(t, func() bool {
		return fixture.server.openStreams.Load() == 0
	}, time.Second, 10*time.Millisecond, "the idle stream did not release its slot")
}

func TestStreamRejectsIdleStreamBeyondLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStreams = 1
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	// The first stream occupies the only slot.
	stream := fixture.open(t, validToken, fixture.resourceName)
	_ = recv(t, stream)

	// A second stream that never sends a request must still hit the cap at
	// open time, before authentication could ever run.
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken)
	idle, err := fixture.client.StreamExtensionConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code := recvCode(t, idle); code != codes.ResourceExhausted {
		t.Fatalf("code = %v, want ResourceExhausted", code)
	}
	if got := testutil.ToFloat64(fixture.metrics.streamLimitRejections); got != 1 {
		t.Fatalf("stream limit rejections = %v, want 1", got)
	}
}

func TestStreamRejectsPerIdentityBeyondLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStreams = 2
	limits.MaxStreamsPerIdentity = 1
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	stream := fixture.open(t, validToken, fixture.resourceName)
	_ = recv(t, stream)

	// The open-stream cap allows the second stream, but the per-identity cap
	// rejects it when it registers.
	second := fixture.open(t, validToken, fixture.resourceName)
	if code := recvCode(t, second); code != codes.ResourceExhausted {
		t.Fatalf("code = %v, want ResourceExhausted", code)
	}
	if got := testutil.ToFloat64(fixture.metrics.streamLimitRejections); got != 1 {
		t.Fatalf("stream limit rejections = %v, want 1", got)
	}
}

func TestAuthSuccessIsNotCountedAsFailure(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	// A rejected token is a failure; a successful authentication is not.
	invalid := fixture.open(t, otherToken, fixture.resourceName)
	if code := recvCode(t, invalid); code != codes.Unauthenticated {
		t.Fatalf("invalid token code = %v, want Unauthenticated", code)
	}
	stream := fixture.open(t, validToken, fixture.resourceName)
	_ = recv(t, stream)

	if got := testutil.ToFloat64(fixture.metrics.authSuccesses); got != 1 {
		t.Fatalf("auth successes = %v, want 1", got)
	}
	if got := testutil.ToFloat64(fixture.metrics.authFailures.WithLabelValues("error")); got != 1 {
		t.Fatalf("auth failures = %v, want 1", got)
	}
	if got := testutil.ToFloat64(fixture.metrics.authFailures.WithLabelValues("authenticated")); got != 0 {
		t.Fatalf("authenticated counted as failure = %v, want 0", got)
	}
}

func TestAuthFailureStormIsRateLimited(t *testing.T) {
	limits := DefaultLimits()
	limits.AuthFailureRate = 1
	limits.AuthFailureBurst = 2
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	tokens := []string{"invalid-1", "invalid-2", "invalid-3", "invalid-4", "invalid-5"}
	for i, token := range tokens {
		stream := fixture.open(t, token, fixture.resourceName)
		want := codes.Unauthenticated
		if i >= limits.AuthFailureBurst {
			want = codes.ResourceExhausted
		}
		if code := recvCode(t, stream); code != want {
			t.Fatalf("attempt %d code = %v, want %v", i+1, code, want)
		}
	}

	if got := len(fixture.fakeClient.Actions()); got != limits.AuthFailureBurst {
		t.Fatalf("TokenReview calls = %d, want %d", got, limits.AuthFailureBurst)
	}
	if got := testutil.ToFloat64(fixture.metrics.authThrottled); got != float64(len(tokens)-limits.AuthFailureBurst) {
		t.Fatalf("throttled authentications = %v, want %d", got, len(tokens)-limits.AuthFailureBurst)
	}
}

func TestAuthFailureLimiterIsPerPeerAndRecovers(t *testing.T) {
	limits := DefaultLimits()
	limits.AuthFailureRate = 1
	limits.AuthFailureBurst = 1
	fixture := newStreamFixtureWithLimits(t, limits)
	now := time.Unix(1_000_000, 0)
	fixture.server.Auth.failureLimiter.now = func() time.Time { return now }

	firstPeer := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1001}
	if _, err := fixture.server.Auth.Authenticate(authContextWithPeer(validToken, firstPeer), validToken); err != nil {
		t.Fatalf("valid token error = %v", err)
	}
	_, err := fixture.server.Auth.Authenticate(authContextWithPeer("invalid-1", firstPeer), "invalid-1")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("first invalid token error = %v, want ErrUnauthenticated", err)
	}

	// A new source port is the same peer and cannot reset its failure budget.
	samePeer := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1002}
	_, err = fixture.server.Auth.Authenticate(authContextWithPeer("invalid-2", samePeer), "invalid-2")
	if !errors.Is(err, ErrAuthRateLimited) {
		t.Fatalf("second invalid token error = %v, want ErrAuthRateLimited", err)
	}

	otherPeer := &net.TCPAddr{IP: net.ParseIP("192.0.2.2"), Port: 1001}
	fixture.server.Auth.cache.Delete(authenticationCacheKey(validToken, ""))
	if _, err := fixture.server.Auth.Authenticate(authContextWithPeer(validToken, otherPeer), validToken); err != nil {
		t.Fatalf("valid token from another peer error = %v", err)
	}

	now = now.Add(time.Second)
	_, err = fixture.server.Auth.Authenticate(authContextWithPeer("invalid-3", samePeer), "invalid-3")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("invalid token after refill error = %v, want ErrUnauthenticated", err)
	}
}

// A review failure that is not an authentication rejection (here, an
// API-server error) must not consume the peer's bucket: only
// ErrUnauthenticated, wrapped or not, counts as a limiter failure.
func TestAuthFailureLimiterIgnoresNonAuthErrors(t *testing.T) {
	limits := DefaultLimits()
	limits.AuthFailureRate = 1
	limits.AuthFailureBurst = 1
	fixture := newStreamFixtureWithLimits(t, limits)
	peerAddr := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1001}

	failAPICall := true
	fixture.fakeClient.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if failAPICall {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	_, err := fixture.server.Auth.Authenticate(authContextWithPeer("doomed", peerAddr), "doomed")
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("review error = %v, want a non-authentication failure", err)
	}

	// The failed review must not have consumed the peer's failure bucket.
	failAPICall = false
	if _, err := fixture.server.Auth.Authenticate(authContextWithPeer("invalid", peerAddr), "invalid"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("next invalid token error = %v, want ErrUnauthenticated rather than a rate limit", err)
	}
}

// The API server answers a rejected token with Authenticated=false and an
// error string: that is an authentication failure and consumes the peer's
// bucket. Only an error next to Authenticated=true is a backend failure that
// leaves the bucket alone.
func TestAuthenticateReviewStatusShapes(t *testing.T) {
	limits := DefaultLimits()
	limits.AuthFailureRate = 1
	limits.AuthFailureBurst = 1
	fixture := newStreamFixtureWithLimits(t, limits)
	now := time.Unix(1_000_000, 0)
	fixture.server.Auth.failureLimiter.now = func() time.Time { return now }
	peerAddr := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1001}

	backendError := true
	fixture.fakeClient.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if backendError {
			return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: true, Error: "authenticator unavailable"}}, nil
		}
		return false, nil, nil
	})
	_, err := fixture.server.Auth.Authenticate(authContextWithPeer("doomed", peerAddr), "doomed")
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("review error = %v, want a backend failure, not a token rejection", err)
	}

	backendError = false
	if _, err := fixture.server.Auth.Authenticate(authContextWithPeer("invalid", peerAddr), "invalid"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("rejected token error = %v, want ErrUnauthenticated rather than a rate limit", err)
	}
	if _, err := fixture.server.Auth.Authenticate(authContextWithPeer("invalid-2", peerAddr), "invalid-2"); !errors.Is(err, ErrAuthRateLimited) {
		t.Fatalf("next token error = %v, want ErrAuthRateLimited once the rejection spent the burst", err)
	}
}

func authContextWithPeer(token string, addr net.Addr) context.Context {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(authorizationHeader, "Bearer "+token))
	return peer.NewContext(ctx, &peer.Peer{Addr: addr})
}

type recvResult struct {
	response *discoveryv3.DiscoveryResponse
	err      error
}

// startRecv starts the single permitted concurrent Recv for a stream.
func startRecv(stream extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient) <-chan recvResult {
	results := make(chan recvResult, 1)
	go func() {
		response, err := stream.Recv()
		results <- recvResult{response: response, err: err}
	}()
	return results
}

func expectNoResult(t *testing.T, results <-chan recvResult) {
	t.Helper()
	select {
	case result := <-results:
		t.Fatalf("Recv() = %+v, %v; want no response", result.response, result.err)
	case <-time.After(200 * time.Millisecond):
	}
}

func expectResult(t *testing.T, results <-chan recvResult) *discoveryv3.DiscoveryResponse {
	t.Helper()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("Recv() error = %v", result.err)
		}
		return result.response
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a response")
		return nil
	}
}

// A rejection replaces the retry that still waits: the proxy has rejected newer
// bytes since, and no other retry would cover them.
func TestResubscribeRejectedReplacesThePendingRetry(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	entry := &serverStream{streamID: 41, session: &StreamSession{ID: 41}}

	fixture.server.resubscribeRejected(entry, fixture.resourceName, "")
	entry.mu.Lock()
	first := entry.retryTimer
	entry.mu.Unlock()
	require.NotNil(t, first)

	fixture.server.resubscribeRejected(entry, fixture.resourceName, "")
	entry.mu.Lock()
	require.NotNil(t, entry.retryTimer, "a later rejection must schedule a retry")
	require.NotSame(t, first, entry.retryTimer, "a later rejection must schedule its own retry")
	require.False(t, first.Stop(), "the replaced retry must not fire")
	entry.cancelRetryLocked()
	entry.mu.Unlock()
}

// A retry resets the stream it was scheduled for while the stream is open, and
// does nothing once the stream has closed. The test runs the retry itself, where
// its timer would, so that the stream is closed or open when it does.
func TestRejectedRetryResetsOnlyAnOpenStream(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	open := func(id int64) (*serverStream, *StreamSession) {
		session := fixture.streams.Register(t.Context(), fixture.resourceName, fixture.identity, validToken)
		t.Cleanup(func() { fixture.streams.Unregister(session.ID) })
		entry := &serverStream{streamID: id, session: session}
		fixture.server.streams.Store(id, entry)
		fixture.server.resubscribeRejected(entry, fixture.resourceName, "")
		return entry, session
	}

	opened, session := open(42)
	fixture.server.runRejectedRetry(opened)
	require.Eventually(t, func() bool { return context.Cause(session.Context()) == ErrResubscribe }, 5*time.Second, 5*time.Millisecond)

	closed, session := open(43)
	fixture.server.onStreamClosed(closed.streamID, nil)
	fixture.server.runRejectedRetry(closed)
	require.NoError(t, session.Context().Err(), "a closed stream must not be reset")
}

// retryPending reports whether a stream waits out the delay of a retry.
func (f *streamFixture) retryPending() bool {
	pending := false
	f.server.streams.Range(func(_, value any) bool {
		entry := value.(*serverStream)
		entry.mu.Lock()
		defer entry.mu.Unlock()
		pending = pending || entry.retryTimer != nil
		return true
	})
	return pending
}

// A rejection is parked, not answered: however many a stream sends, the cache
// sends nothing back and the response budget stays untouched.
func TestStreamParksRejections(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	// A pod's budget is one burst of 16 full responses, which answering this
	// many rejections would exhaust.
	stream := fixture.open(t, podToken("pod-1"), fixture.resourceName)
	first := recv(t, stream)
	const rejections = 100
	for range rejections {
		send(t, stream, &discoveryv3.DiscoveryRequest{
			ResourceNames: []string{fixture.resourceName},
			ResponseNonce: first.GetNonce(),
			ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
		})
	}
	// The first rejection is handled, and the others repeat it.
	fixture.awaitRejections(t, 1, rejections-1)

	expectNoResult(t, startRecv(stream))
	fixture.server.streams.Range(func(_, value any) bool {
		entry := value.(*serverStream)
		entry.mu.Lock()
		defer entry.mu.Unlock()
		require.Equal(t, first.GetNonce(), entry.lastNonce, "the cache must not have sent another response")
		return true
	})
	require.Zero(t, fixture.watches(fixture.node(0)), "a rejection must not watch the Engine's snapshot")
	require.Equal(t, 1, fixture.watches(parkedNode), "each rejection replaces the stream's parked watch")
	require.True(t, fixture.retryPending(), "the stream stays parked while its retry waits")
	require.Zero(t, testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalResponseBudget)))
}

// New content ends the wait for a rejected configuration's retry at once: the
// stream is reset so the proxy receives it without waiting out the delay, and
// publishing the same content again does not.
func TestStreamRejectionWakesOnNewContent(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	fixture.awaitWritten(t)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		ResponseNonce: first.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	})
	require.Eventually(t, fixture.retryPending, 5*time.Second, 5*time.Millisecond)

	results := startRecv(stream)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	expectNoResult(t, results)

	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	select {
	case result := <-results:
		require.Equal(t, codes.Unavailable, status.Code(result.err))
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was not reset by new content")
	}
	// The rejected tag was retired, so the proxy receives the new content under
	// the next generation.
	require.Equal(t, fixture.wire("v2", 1), recv(t, fixture.open(t, validToken, fixture.resourceName)).GetVersionInfo())
}

// A proxy that reports a tag of the Engine's current content when it connects is
// answered with those bytes again, whichever process sent them first, so it keeps
// its filter across a reconnect. Any other version gets the current tag.
func TestStreamServesTheTagAProxyReports(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	require.True(t, fixture.store.Advance(testNamespace+"/"+streamEngine, tag{epoch: fixture.store.epoch}))
	elsewhere := wireVersion("sha256:v1", tag{epoch: otherProcessEpoch, gen: 6})
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{name: "a tag of another process", version: elsewhere, want: elsewhere},
		{name: "an earlier tag of this process", version: fixture.wire("v1", 0), want: fixture.wire("v1", 0)},
		{name: "no version", want: fixture.wire("v1", 1)},
		{name: "the tag of other content", version: wireVersion("sha256:v0", tag{epoch: otherProcessEpoch}), want: fixture.wire("v1", 1)},
		{name: "a bare content version", version: "sha256:v1", want: fixture.wire("v1", 1)},
		{name: "a retry version of an earlier release", version: "sha256:v1.retry2", want: fixture.wire("v1", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each case is a proxy of its own: a proxy that reconnects without
			// acknowledging what it was sent is not sent that again.
			stream := fixture.openClaiming(t, "reconnecting-"+tt.name, tt.version)
			require.Equal(t, tt.want, recv(t, stream).GetVersionInfo())
		})
	}
}

// A stream that ends gives its tag snapshot back, which is dropped when no
// other stream holds it and the Engine has moved on.
func TestStreamCloseReleasesItsTagSnapshot(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	elsewhere := tag{epoch: otherProcessEpoch, gen: 6}
	cancel, recvNext := fixture.openRequest(t, &discoveryv3.DiscoveryRequest{
		Node: &corev3.Node{Id: "leaving"}, ResourceNames: []string{fixture.resourceName}, VersionInfo: wireVersion("sha256:v1", elsewhere),
	})
	_, err := recvNext()
	require.NoError(t, err)
	node := heldTag{tag: elsewhere, render: fixture.renders()}.node(testNamespace + "/" + streamEngine)
	_, err = fixture.store.cache.GetSnapshot(node)
	require.NoError(t, err)

	cancel()
	require.Eventually(t, func() bool {
		_, err := fixture.store.cache.GetSnapshot(node)
		return err != nil
	}, 5*time.Second, 5*time.Millisecond)
}

// renders is how many times content was published for the fixture's Engine.
func (f *streamFixture) renders() uint64 {
	f.store.mu.RLock()
	defer f.store.mu.RUnlock()
	return f.store.engines[testNamespace+"/"+streamEngine].renders
}

// heldTags is how many tag snapshots the fixture's Engine holds.
func (f *streamFixture) heldTags() int {
	f.store.mu.RLock()
	defer f.store.mu.RUnlock()
	return len(f.store.engines[testNamespace+"/"+streamEngine].tags)
}

// openClaiming opens a stream of the proxy with node ID proxyID whose first
// request reports version as the one the proxy last accepted.
func (f *streamFixture) openClaiming(t *testing.T, proxyID, version string) extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient {
	t.Helper()
	ctx := metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer "+validToken)
	stream, err := f.client.StreamExtensionConfigs(ctx)
	require.NoError(t, err)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		Node: &corev3.Node{Id: proxyID}, ResourceNames: []string{f.resourceName}, VersionInfo: version,
	})
	return stream
}

// A proxy that connects again without having acknowledged what it was sent is
// not sent that again, whether or not its rejection reached the server: Envoy
// accepts bytes it rejected, when it is sent them again, without building them.
// A proxy that acknowledged is sent the tag it reports.
func TestStreamNeverResendsAnUnacknowledgedTag(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	for gen := range uint64(3) {
		stream := fixture.openAs(t, "proxy", fixture.resourceName)
		require.Equal(t, fixture.wire("v1", gen), recv(t, stream).GetVersionInfo(), "connection %d", gen)
	}

	accepting := fixture.openClaiming(t, "accepting", "")
	accepted := recv(t, accepting)
	require.Equal(t, fixture.wire("v1", 2), accepted.GetVersionInfo(), "a proxy that was sent nothing is sent the current tag")
	send(t, accepting, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: accepted.GetVersionInfo(), ResponseNonce: accepted.GetNonce(),
	})
	fixture.awaitAcknowledged(t, "accepting")
	again := fixture.openClaiming(t, "accepting", accepted.GetVersionInfo())
	require.Equal(t, fixture.wire("v1", 2), recv(t, again).GetVersionInfo())
}

// A rejection replayed on a new stream names no response; the tag the proxy was
// last sent is the one it rejected.
func TestStreamReplayedRejectionNamesTheTagItWasSent(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	first := fixture.openAs(t, "proxy", fixture.resourceName)
	require.Equal(t, fixture.wire("v1", 0), recv(t, first).GetVersionInfo())
	fixture.awaitWritten(t)

	ctx := metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer "+validToken)
	replay, err := fixture.client.StreamExtensionConfigs(ctx)
	require.NoError(t, err)
	send(t, replay, &discoveryv3.DiscoveryRequest{
		Node: &corev3.Node{Id: "proxy"}, ResourceNames: []string{fixture.resourceName},
		ErrorDetail: status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	})
	require.Eventually(t, func() bool { return fixture.generation(testNamespace+"/"+streamEngine) == 1 }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, 1, fixture.record("proxy").attempts)
}

// A rejection of content the Engine no longer has does not wait out the retry
// delay: the proxy is sent the new content at once.
func TestStreamRejectionOfOldContentIsRetriedAtOnce(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	fixture.awaitWritten(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))

	results := startRecv(stream)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		ResponseNonce: first.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	})
	select {
	case result := <-results:
		require.Equal(t, codes.Unavailable, status.Code(result.err))
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was not reset although the Engine has new content")
	}
	require.Equal(t, fixture.wire("v2", 1), recv(t, fixture.open(t, validToken, fixture.resourceName)).GetVersionInfo())
}

// A rejection of a response other than the last one sent on the stream is stale:
// the proxy has been sent other bytes since, which neither a reset of the stream
// nor a longer back-off answers. go-control-plane skips such a request too.
func TestStreamIgnoresARejectionOfAnEarlierResponse(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: first.GetVersionInfo(), ResponseNonce: first.GetNonce(),
	})
	fixture.awaitAcknowledged(t, fixture.proxyID)
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v2")))
	second := recv(t, stream)
	require.NotEqual(t, first.GetNonce(), second.GetNonce())

	// The rejection of the first response arrives after the second was pushed.
	// Requests are handled in order: once the acknowledgement of the second has
	// been, the rejection has.
	send(t, stream, rejectionRequest(fixture.resourceName, first.GetNonce()))
	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: second.GetVersionInfo(), ResponseNonce: second.GetNonce(),
	})
	fixture.awaitAcknowledged(t, fixture.proxyID)
	fixture.awaitRejections(t, 0, 1)
	require.False(t, fixture.retryPending(), "the stream is not reset for a rejection it has moved past")
	require.Zero(t, fixture.record(fixture.proxyID).attempts, "the back-off is not lengthened by it")
	require.Zero(t, fixture.generation(testNamespace+"/"+streamEngine), "no tag is retired for it")

	// The stream still receives new content.
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v3")))
	require.Equal(t, fixture.wire("v3", 0), recv(t, stream).GetVersionInfo())
}

// rejectionRequest is a proxy's rejection of the response with nonce.
func rejectionRequest(resourceName, nonce string) *discoveryv3.DiscoveryRequest {
	return &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{resourceName},
		ResponseNonce: nonce,
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}
}

// awaitRejections waits until the server has handled rejected rejections that were
// not ignored, and ignored that were. Requests are handled in order, so the count
// of the last tells when the ones before it have been.
func (f *streamFixture) awaitRejections(t *testing.T, rejected, ignored int) {
	t.Helper()
	counted := func(reason string) float64 { return testutil.ToFloat64(f.metrics.nacks.WithLabelValues(reason)) }
	require.Eventually(t, func() bool {
		return counted("rejected") == float64(rejected) && counted("ignored") == float64(ignored)
	}, 60*time.Second, 5*time.Millisecond, "%d rejections handled and %d ignored", rejected, ignored)
}

// A proxy that repeats a rejection is answered once: the retry that waits
// answers the rest, so a stream cannot make the operator count, log and
// reschedule it again and again, however many it sends.
func TestStreamIgnoresARepeatedRejection(t *testing.T) {
	fixture := newStreamFixture(t)
	var logs logLines
	fixture.server.Logger = logs.logger()
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	fixture.awaitWritten(t)

	const rejections = 20000
	for range rejections {
		send(t, stream, rejectionRequest(fixture.resourceName, first.GetNonce()))
	}
	fixture.awaitRejections(t, 1, rejections-1)

	require.Len(t, logs.with("resubscribing proxy"), 1, "one rejection is logged")
	require.Contains(t, logs.with("resubscribing proxy")[0], `"proxy"="`+fixture.proxyID+`"`)
	require.Equal(t, 1, fixture.record(fixture.proxyID).attempts, "counted")
	require.Equal(t, uint64(1), fixture.generation(testNamespace+"/"+streamEngine), "and retired")
	require.True(t, fixture.retryPending())

	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: first.GetVersionInfo(), ResponseNonce: first.GetNonce(),
	})
	fixture.awaitAcknowledged(t, fixture.proxyID)
	require.Len(t, logs.with("accepted by proxy"), 1)
	require.Contains(t, logs.with("accepted by proxy")[0], `"proxy"="`+fixture.proxyID+`"`)
}

// A stream that was sent no response has none for a rejection to name, whatever
// nonce it carries: changing the nonce does not make a rejection new. The first
// is, as the one replayed from an earlier stream is, and so is the rejection of a
// response sent later, even when the nonce it carries was guessed.
func TestStreamIgnoresRejectionsWithOtherNoncesBeforeAnyResponse(t *testing.T) {
	fixture := newStreamFixture(t)
	var logs logLines
	fixture.server.Logger = logs.logger()
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stream, err := fixture.client.StreamExtensionConfigs(metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer "+validToken))
	require.NoError(t, err)

	// The first nonce go-control-plane gives a stream is "1".
	const rejections = 20000
	first := rejectionRequest(fixture.resourceName, "1")
	first.Node = &corev3.Node{Id: fixture.proxyID}
	send(t, stream, first)
	for i := 1; i < rejections; i++ {
		send(t, stream, rejectionRequest(fixture.resourceName, strconv.Itoa(i)))
	}
	fixture.awaitRejections(t, 1, rejections-1)
	require.Len(t, logs.with("resubscribing proxy"), 1, "one rejection is logged")
	require.True(t, fixture.retryPending())

	// The response that is sent next is rejected like any other.
	send(t, stream, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}})
	response := recv(t, stream)
	require.Equal(t, "1", response.GetNonce())
	send(t, stream, rejectionRequest(fixture.resourceName, response.GetNonce()))
	fixture.awaitRejections(t, 2, rejections-1)
	require.Len(t, logs.with("resubscribing proxy"), 2)
}

// ignoresRejection tells a rejection the stream has already answered, or that
// answers no response it sent, from one that is new.
func TestIgnoresRejection(t *testing.T) {
	tests := []struct {
		name        string
		lastNonce   string // of the last response sent on the stream
		retryWaits  bool
		nackedNonce string // the last nonce when the retry was armed
		nonce       string // of the rejection
		want        bool
	}{
		{name: "a rejection of the last response", lastNonce: "2", nonce: "2"},
		{name: "a rejection of an earlier response", lastNonce: "2", nonce: "1", want: true},
		{name: "a rejection with no nonce after a response was sent", lastNonce: "2", want: true},
		{name: "a rejection replayed on a new stream", nonce: ""},
		{name: "a rejection of the last response again while its retry waits", lastNonce: "2", retryWaits: true, nackedNonce: "2", nonce: "2", want: true},
		{name: "a rejection of a newer response while a retry waits", lastNonce: "3", retryWaits: true, nackedNonce: "2", nonce: "3"},
		{name: "a rejection of the first response while a replayed rejection's retry waits", lastNonce: "1", retryWaits: true, nonce: "1"},
		{name: "a rejection replayed again while its retry waits", retryWaits: true, want: true},
		{name: "a rejection with another nonce on a stream sent no response, while a retry waits", retryWaits: true, nonce: "n1", want: true},
		{name: "a rejection with a nonce on a stream sent no response, with no retry waiting", nonce: "n1"},
		{name: "a rejection of the last response whose retry has run", lastNonce: "2", nackedNonce: "2", nonce: "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &serverStream{lastNonce: tt.lastNonce, nackedNonce: tt.nackedNonce}
			if tt.retryWaits {
				entry.retryTimer = time.NewTimer(time.Hour)
				defer entry.retryTimer.Stop()
			}
			require.Equal(t, tt.want, entry.ignoresRejection(rejectionRequest("", tt.nonce)))
		})
	}
}

// A rejection replayed on a new stream names no response; the proxy rejected what
// it was last sent, and when the Engine has other content since, the proxy is
// sent it at once rather than after the back-off.
func TestStreamReplayedRejectionOfOldContentIsRetriedAtOnce(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	first := fixture.openAs(t, "proxy", fixture.resourceName)
	require.Equal(t, fixture.wire("v1", 0), recv(t, first).GetVersionInfo())
	fixture.awaitWritten(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))

	ctx := metadata.AppendToOutgoingContext(t.Context(), "authorization", "Bearer "+validToken)
	replay, err := fixture.client.StreamExtensionConfigs(ctx)
	require.NoError(t, err)
	send(t, replay, &discoveryv3.DiscoveryRequest{
		Node: &corev3.Node{Id: "proxy"}, ResourceNames: []string{fixture.resourceName},
		ErrorDetail: status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	})
	select {
	case result := <-startRecv(replay):
		require.Equal(t, codes.Unavailable, status.Code(result.err))
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was not reset although the Engine has new content")
	}
	require.Zero(t, fixture.generation(testNamespace+"/"+streamEngine), "bytes of content the Engine has left are not retired")
}

// A stale rejection after an acknowledgement is ignored without parking the stream:
// the watch go-control-plane made for the acknowledgement does not know the nonce it
// would skip the request by, and would be replaced by a parked one that nothing
// resets, which leaves the stream without updates until it ends.
func TestStreamKeepsItsWatchForAStaleRejectionAfterAnAcknowledgement(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: first.GetVersionInfo(), ResponseNonce: first.GetNonce(),
	})
	fixture.awaitAcknowledged(t, fixture.proxyID)

	send(t, stream, rejectionRequest(fixture.resourceName, "an earlier response"))
	fixture.awaitRejections(t, 0, 1)
	require.False(t, fixture.retryPending())
	require.Equal(t, 1, fixture.watches(fixture.node(0)), "the stream still watches its snapshot")
	require.Zero(t, fixture.watches(parkedNode), "and is not parked")

	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v2")))
	require.Equal(t, fixture.wire("v2", 0), expectResult(t, startRecv(stream)).GetVersionInfo())
}

// The record of a proxy that was sent a tag and did not acknowledge it can be
// evicted by another proxy's response, which advances the Engine, between the
// proxy's lease and the check of its record. The check then finds nothing, and the
// tag that was advanced past is the one the proxy may have rejected: it is not
// served, and the lease is given back for the Engine's current tag.
func TestLeaseForDoesNotServeATagTheEngineWasAdvancedPastMeanwhile(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	engineKey := testNamespace + "/" + streamEngine
	rejected := tag{epoch: fixture.store.epoch}
	entry := &serverStream{engineKey: engineKey, proxyKey: "rejecting-proxy"}
	records := fixture.server.deliveries
	records.sent(entry.proxyKey, engineKey, "sha256:v1", rejected)

	// leaseFor leases and then reads the records, which the test holds.
	records.mu.Lock()
	leased := make(chan tagLease, 1)
	go func() {
		lease, err := fixture.server.leaseFor(entry, "")
		assert.NoError(t, err)
		leased <- lease
	}()
	require.Eventually(t, func() bool { return fixture.heldTags() == 1 }, 5*time.Second, time.Millisecond)

	// Another proxy's response drops the record, and the Engine moves on.
	records.order.Remove(records.byKey[entry.proxyKey])
	delete(records.byKey, entry.proxyKey)
	require.True(t, fixture.store.Advance(engineKey, rejected))
	records.mu.Unlock()

	select {
	case lease := <-leased:
		require.Equal(t, tag{epoch: fixture.store.epoch, gen: 1}, lease.tag, "the tag the proxy may have rejected was not served")
	case <-time.After(5 * time.Second):
		t.Fatal("leaseFor did not return")
	}
	require.Equal(t, 1, fixture.heldTags(), "the lease on the tag that was advanced past was given back")
}

// A publication wakes the streams that wait on a rejection of other content. A
// stream that rejected the content being published, which it can receive and
// reject before the notice of its publication runs, keeps the delay its module
// fetch needs.
func TestWakeRejectedSparesAStreamThatRejectedTheContentPublished(t *testing.T) {
	fixture := newStreamFixture(t)
	engineKey := testNamespace + "/" + streamEngine
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v2")))
	woken := make(chan string, 2)
	wait := func(id int64, version string) {
		entry := &serverStream{streamID: id, retryEngine: engineKey, retryVersion: version}
		entry.retryTimer = time.AfterFunc(time.Hour, func() { woken <- version })
		t.Cleanup(func() { entry.retryTimer.Stop() })
		fixture.server.streams.Store(id, entry)
	}
	wait(1, "sha256:v1")
	wait(2, "sha256:v2")

	fixture.server.wakeRejected(engineKey)
	select {
	case version := <-woken:
		require.Equal(t, "sha256:v1", version, "the stream that rejected the earlier content is woken")
	case <-time.After(5 * time.Second):
		t.Fatal("the stream that rejected the earlier content was not woken")
	}
	select {
	case version := <-woken:
		t.Fatalf("the stream that rejected %s was woken", version)
	case <-time.After(50 * time.Millisecond):
	}
}

// Content published while a rejection's retry is being scheduled is not missed:
// either the retry was already waiting when the content arrived, and the content
// ended its wait, or the scheduling saw the content. Either way the proxy does not
// wait out the back-off.
func TestResubscribeRejectedSeesContentPublishedWhileItSchedules(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	engineKey := testNamespace + "/" + streamEngine
	current := "v0"
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, current)))
	for i := range 300 {
		proxyID := fmt.Sprintf("proxy-%d", i)
		session := fixture.streams.Register(t.Context(), fixture.resourceName, fixture.identity, validToken)
		entry := &serverStream{streamID: int64(i), session: session, identity: fixture.identity, proxyID: proxyID}
		fixture.server.streams.Store(entry.streamID, entry)
		fixture.server.deliveries.sent(proxyCacheKey(engineKey, fixture.identity, proxyID), engineKey, "sha256:"+current, tag{epoch: fixture.store.epoch})

		next := fmt.Sprintf("v%d", i+1)
		snapshot := fixture.snapshotWithContent(t, next)
		published := make(chan struct{})
		go func() {
			defer close(published)
			assert.NoError(t, fixture.store.Replace(snapshot))
		}()
		fixture.server.resubscribeRejected(entry, fixture.resourceName, "")
		<-published
		current = next

		require.Eventually(t, func() bool { return context.Cause(session.Context()) == ErrResubscribe }, 5*time.Second, time.Millisecond, "proxy %d waited out the back-off", i)
		fixture.server.streams.Delete(entry.streamID)
		fixture.streams.Unregister(session.ID)
	}
}

// Only a response writes a delivery record. Streams that send nothing but
// rejections, each under a node ID of its own, fill no record table and so evict
// no proxy's record: a proxy still has the tag it was sent on its record, and the
// Engine does not move on for the records that would have been dropped.
func TestStreamRejectionsUnderNewNodeIDsEvictNoRecords(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	limits.MaxStreams = 16
	fixture := newStreamFixtureWithLimits(t, limits)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	engineKey := testNamespace + "/" + streamEngine
	held := tag{epoch: fixture.store.epoch}
	fixture.server.deliveries.sent("victim", engineKey, "sha256:v1", held)

	nacks := fixture.metrics.nacks.WithLabelValues("rejected")
	for i := range recordsPerStream*limits.MaxStreams + 1 {
		cancel, _ := fixture.openRequest(t, &discoveryv3.DiscoveryRequest{
			Node:          &corev3.Node{Id: fmt.Sprintf("churn-%d", i)},
			ResourceNames: []string{fixture.resourceName},
			ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
		})
		require.Eventually(t, func() bool { return testutil.ToFloat64(nacks) == float64(i+1) }, 5*time.Second, time.Millisecond)
		cancel()
	}

	require.True(t, fixture.server.deliveries.unacknowledged("victim", held), "the proxy's record is kept")
	fixture.server.deliveries.mu.Lock()
	records := fixture.server.deliveries.order.Len()
	fixture.server.deliveries.mu.Unlock()
	require.Equal(t, 1, records, "the rejections wrote no record")
	require.Zero(t, fixture.generation(engineKey), "no record was dropped, so the Engine did not move on")
	require.Zero(t, testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalResponseBudget)))
}

// Proxies that report no node ID are told apart by the pod their token is bound
// to, so one's acknowledgement is not another's.
func TestStreamProxiesWithoutNodeIDsAreTold(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	for _, pod := range []string{"pod-a", "pod-b"} {
		cancel, recvNext := fixture.openRequestAs(t, podToken(pod), &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}})
		defer cancel()
		_, err := recvNext()
		require.NoError(t, err)
	}

	fixture.server.deliveries.mu.Lock()
	defer fixture.server.deliveries.mu.Unlock()
	require.Equal(t, 2, fixture.server.deliveries.order.Len())
}
