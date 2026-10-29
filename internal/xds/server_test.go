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
	"net"
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
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

const (
	validToken   = "valid-token"
	otherToken   = "other-token"
	streamEngine = "checkout"
)

type streamFixture struct {
	server        *Server
	store         *SnapshotStore
	authz         *AuthzIndex
	streams       *StreamRegistry
	metrics       *Metrics
	conn          *grpc.ClientConn
	client        extensionv3.ExtensionConfigDiscoveryServiceClient
	resourceName  string
	identity      Identity
	setTokenValid func(bool)
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	return newStreamFixtureWithLimits(t, limits)
}

func newStreamFixtureWithLimits(t *testing.T, limits Limits) *streamFixture {
	t.Helper()

	identity := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}
	resourceName := ResourceName(testNamespace, streamEngine, types.UID("engine-uid"))

	var tokenMu sync.Mutex
	tokenValid := true
	fakeClient := k8sfake.NewSimpleClientset()
	fakeClient.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		request := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		tokenMu.Lock()
		valid := tokenValid
		tokenMu.Unlock()
		if !valid || request.Spec.Token != validToken {
			return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: false}}, nil
		}
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true,
			User:          authv1.UserInfo{Username: "system:serviceaccount:" + identity.Namespace + ":" + identity.Name, UID: identity.UID},
		}}, nil
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
		RefreshedAt:  time.Now(),
	}

	auth := authzTokenReviewer(fakeClient)
	streams = NewStreamRegistry(auth, authz, limits, metrics)

	server := NewServer(store, auth, authz, streams, limits, metrics)

	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(grpc.StreamInterceptor(server.StreamInterceptor()))
	server.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &streamFixture{
		server:       server,
		store:        store,
		authz:        authz,
		streams:      streams,
		metrics:      metrics,
		conn:         conn,
		client:       extensionv3.NewExtensionConfigDiscoveryServiceClient(conn),
		resourceName: resourceName,
		identity:     identity,
		setTokenValid: func(valid bool) {
			tokenMu.Lock()
			tokenValid = valid
			tokenMu.Unlock()
		},
	}
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
	if err := stream.Send(&discoveryv3.DiscoveryRequest{ResourceNames: names}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	return stream
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
	if first.GetVersionInfo() != "sha256:v1" {
		t.Fatalf("version_info = %q, want sha256:v1", first.GetVersionInfo())
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
	if second.GetVersionInfo() != "sha256:v2" || second.GetNonce() != "2" {
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
	if second.GetVersionInfo() != "sha256:v2" {
		t.Fatalf("version_info = %q, want sha256:v2", second.GetVersionInfo())
	}

	// Envoy's NACK carries the previous ACCEPTED version, not the rejected one.
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: second.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	require.Eventually(t, func() bool {
		cached, err := fixture.store.cache.GetSnapshot(testNamespace + "/" + streamEngine)
		return err == nil && cached.GetVersion(resource.ExtensionConfigType) == "sha256:v2.retry1"
	}, time.Second, 5*time.Millisecond)
}

func TestStreamNackRepublishesUnderNewVersion(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	nack := func(stream extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient, resp *discoveryv3.DiscoveryResponse) {
		t.Helper()
		if err := stream.Send(&discoveryv3.DiscoveryRequest{
			ResourceNames: []string{fixture.resourceName},
			VersionInfo:   resp.GetVersionInfo(),
			ResponseNonce: resp.GetNonce(),
			ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	serveVersion := func() string {
		stream := fixture.open(t, validToken, fixture.resourceName)
		return recv(t, stream).GetVersionInfo()
	}
	cachedVersion := func() string {
		cached, err := fixture.store.cache.GetSnapshot(testNamespace + "/" + streamEngine)
		if err != nil {
			return ""
		}
		return cached.GetVersion(resource.ExtensionConfigType)
	}

	// A cold proxy rejects the configuration because the remote module is not
	// cached. Envoy never re-applies a version it rejected, so the operator
	// publishes a derived version and closes the stream for a resubscribe.
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	if got := first.GetVersionInfo(); got != "sha256:v1" {
		t.Fatalf("version_info = %q, want sha256:v1", got)
	}
	nack(stream, first)
	_ = recvCode(t, stream)
	require.Eventually(t, func() bool { return cachedVersion() == "sha256:v1.retry1" }, time.Second, 5*time.Millisecond)
	if got := serveVersion(); got != "sha256:v1.retry1" {
		t.Fatalf("resubscribed version = %q, want sha256:v1.retry1", got)
	}

	// A second rejection bumps again, and the accepted version never changes so
	// the publication loop still coalesces.
	stream = fixture.open(t, validToken, fixture.resourceName)
	second := recv(t, stream)
	if got := second.GetVersionInfo(); got != "sha256:v1.retry1" {
		t.Fatalf("version_info = %q, want sha256:v1.retry1", got)
	}
	nack(stream, second)
	_ = recvCode(t, stream)
	require.Eventually(t, func() bool { return cachedVersion() == "sha256:v1.retry2" }, time.Second, 5*time.Millisecond)

	snapshot, ok := fixture.store.Get(testNamespace + "/" + streamEngine)
	require.True(t, ok)
	if snapshot.Version != "sha256:v1" {
		t.Fatalf("accepted version = %q, want sha256:v1", snapshot.Version)
	}
}

func TestStreamNackRetryBudgetExpires(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	// A retry budget that already ran out its window must not bump again.
	fixture.exhaustNackBudget(t, 1, time.Now().Add(-time.Hour))

	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: first.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}

	// No close, no bump: the proxy stays on the rejected configuration.
	expectNoResult(t, startRecv(stream))
	cached, err := fixture.store.cache.GetSnapshot(testNamespace + "/" + streamEngine)
	require.NoError(t, err)
	if got := cached.GetVersion(resource.ExtensionConfigType); got != "sha256:v1" {
		t.Fatalf("cached version = %q, want sha256:v1 (no retry published)", got)
	}
}

func TestStreamNackRepublishFailureKeepsStream(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	// A resource that is not a decodable WASM config cannot be retried.
	snapshot := fixture.snapshot(t, "v1")
	snapshot.Resource = &corev3.TypedExtensionConfig{Name: fixture.resourceName}
	require.NoError(t, fixture.store.Replace(snapshot))

	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: first.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}

	expectNoResult(t, startRecv(stream))
	cached, err := fixture.store.cache.GetSnapshot(testNamespace + "/" + streamEngine)
	require.NoError(t, err)
	if got := cached.GetVersion(resource.ExtensionConfigType); got != "sha256:v1" {
		t.Fatalf("cached version = %q, want sha256:v1 (retry must not publish)", got)
	}
}

func TestStreamAckResetsNackBudget(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	// The stream's own budget is exhausted for this configuration.
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	key := fixture.exhaustNackBudget(t, nackRetryLimit+1, time.Now())

	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: first.GetNonce(),
	}); err != nil {
		t.Fatal(err)
	}

	// An acknowledgement clears the stream's budget, so a later rejection on
	// the same stream is retried.
	require.Eventually(t, func() bool {
		_, ok := fixture.server.nacks.Load(key)
		return !ok
	}, time.Second, 5*time.Millisecond)

	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: first.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	if code := recvCode(t, stream); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
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
		ResourceNames: []string{fixture.resourceName},
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
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
		if err := stream.Send(&discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}}); err != nil {
			t.Fatal(err)
		}
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
	// No subscription was authorized, so this first-message ACK must not
	// create a watch on the client-supplied Engine key.
	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "victim-ns/victim-engine"},
		ResponseNonce: "spoofed-nonce",
	}); err != nil {
		t.Fatal(err)
	}
	expectNoResult(t, startRecv(stream))
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
	budgets := 0
	fixture.server.nacks.Range(func(_, _ any) bool { budgets++; return true })
	if budgets != 0 {
		t.Fatal("an unauthorized NACK must not consume any retry budget")
	}
	cached, err := fixture.store.cache.GetSnapshot("victim-ns/victim-engine")
	require.NoError(t, err)
	if got := cached.GetVersion(resource.ExtensionConfigType); got != "sha256:victim-v1" {
		t.Fatalf("cached version = %q, want sha256:victim-v1", got)
	}
}

func TestRecordNackLosesNoConcurrentAttempts(t *testing.T) {
	fixture := newStreamFixture(t)
	key := nackKey(fixture.resourceName, 1)
	const workers = 64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fixture.server.recordNack(key, "sha256:v1")
		}()
	}
	wg.Wait()

	raw, ok := fixture.server.nacks.Load(key)
	require.True(t, ok)
	require.Equal(t, workers, raw.(nackState).attempts)
}

// exhaustNackBudget marks the fixture's one subscribed stream as having an
// exhausted retry budget, the way resubscribeRejected would have tracked it,
// and returns the stream's budget key.
func (f *streamFixture) exhaustNackBudget(t *testing.T, attempts int, first time.Time) string {
	t.Helper()
	var key string
	f.server.streams.Range(func(_, value any) bool {
		entry := value.(*serverStream)
		if entry.session != nil && entry.resourceName == f.resourceName {
			key = nackKey(entry.resourceName, entry.session.ID)
			entry.mu.Lock()
			entry.nackKey = key
			entry.mu.Unlock()
			f.server.nacks.Store(key, nackState{version: "sha256:v1", attempts: attempts, first: first})
		}
		return true
	})
	if key == "" {
		t.Fatal("no subscribed stream found")
	}
	return key
}

func TestNackBudgetIsPerStream(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 0
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	// Stream A exhausts its own budget for the resource: no bump, no close.
	streamA := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, streamA)
	keyA := fixture.exhaustNackBudget(t, nackRetryLimit+1, time.Now())
	if err := streamA.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: first.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	expectNoResult(t, startRecv(streamA))
	cached, err := fixture.store.cache.GetSnapshot(testNamespace + "/" + streamEngine)
	require.NoError(t, err)
	if got := cached.GetVersion(resource.ExtensionConfigType); got != "sha256:v1" {
		t.Fatalf("cached version = %q, want sha256:v1 (A must not trigger a retry)", got)
	}

	// Stream B subscribes to the same resource: A's exhausted budget must not
	// block B's retry.
	streamB := fixture.open(t, validToken, fixture.resourceName)
	second := recv(t, streamB)
	if err := streamB.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   second.GetVersionInfo(),
		ResponseNonce: second.GetNonce(),
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	}); err != nil {
		t.Fatal(err)
	}
	if code := recvCode(t, streamB); code != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", code)
	}
	require.Eventually(t, func() bool {
		cached, err := fixture.store.cache.GetSnapshot(testNamespace + "/" + streamEngine)
		return err == nil && cached.GetVersion(resource.ExtensionConfigType) == "sha256:v1.retry1"
	}, time.Second, 5*time.Millisecond)

	// B's closed stream reclaims its budget entry; only A's open stream keeps
	// its exhausted entry.
	require.Eventually(t, func() bool {
		count := 0
		fixture.server.nacks.Range(func(_, _ any) bool { count++; return true })
		return count == 1
	}, time.Second, 5*time.Millisecond)
	if _, ok := fixture.server.nacks.Load(keyA); !ok {
		t.Fatal("the open stream's budget entry must remain until it closes")
	}
}

func TestStreamDoesNotSendAnEmptyResponseOnMiss(t *testing.T) {
	fixture := newStreamFixture(t)
	stream := fixture.open(t, validToken, fixture.resourceName)

	pending := startRecv(stream)
	expectNoResult(t, pending)

	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	response := expectResult(t, pending)
	if response.GetVersionInfo() != "sha256:v1" {
		t.Fatalf("version_info = %q, want sha256:v1", response.GetVersionInfo())
	}
}

func TestStreamRevokedWhenAuthorizationIsRemoved(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	stream := fixture.open(t, validToken, fixture.resourceName)
	_ = recv(t, stream)

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

	fixture.setTokenValid(false)
	fixture.streams.revalidateAll(context.Background())
	if code := recvCode(t, stream); code != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", code)
	}
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
