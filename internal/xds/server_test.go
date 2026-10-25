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
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
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
	conn          *grpc.ClientConn
	client        extensionv3.ExtensionConfigDiscoveryServiceClient
	resourceName  string
	identity      Identity
	setTokenValid func(bool)
}

func newStreamFixture(t *testing.T) *streamFixture {
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
	authz := NewAuthzIndex(nil, time.Hour)
	authz.entries[resourceName] = AuthzEntry{
		ResourceName: resourceName,
		EngineUID:    types.UID("engine-uid"),
		Namespace:    testNamespace,
		EngineName:   streamEngine,
		GatewayName:  testGatewayName,
		Identities:   map[string]string{identity.Key(): identity.UID},
		RefreshedAt:  time.Now(),
	}

	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	auth := authzTokenReviewer(fakeClient)
	streams := NewStreamRegistry(auth, authz, limits, metrics)
	authz.SetRevoker(streams)

	server := &Server{
		Store:   store,
		Auth:    auth,
		Authz:   authz,
		Streams: streams,
		Limits:  limits,
		Metrics: metrics,
	}

	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
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

func (f *streamFixture) snapshot(version string) *Snapshot {
	return &Snapshot{
		ResourceName: f.resourceName,
		Namespace:    testNamespace,
		EngineName:   streamEngine,
		EngineUID:    types.UID("engine-uid"),
		RuleSetHash:  "sha256:" + version,
		Version:      "sha256:" + version,
	}
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
	fixture.store.Replace(fixture.snapshot("v1"))

	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	if first.GetVersionInfo() != "sha256:v1" {
		t.Fatalf("version_info = %q, want sha256:v1", first.GetVersionInfo())
	}
	if first.GetNonce() != "1" {
		t.Fatalf("nonce = %q, want 1", first.GetNonce())
	}
	if first.GetTypeUrl() != TypeURL || len(first.GetResources()) != 1 {
		t.Fatalf("response = %+v", first)
	}

	if err := stream.Send(&discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		VersionInfo:   first.GetVersionInfo(),
		ResponseNonce: first.GetNonce(),
	}); err != nil {
		t.Fatal(err)
	}

	fixture.store.Replace(fixture.snapshot("v2"))
	second := recv(t, stream)
	if second.GetVersionInfo() != "sha256:v2" || second.GetNonce() != "2" {
		t.Fatalf("second response = version %q nonce %q", second.GetVersionInfo(), second.GetNonce())
	}
}

func TestStreamNackDoesNotResendTheSameVersion(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.store.Replace(fixture.snapshot("v1"))

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

	pending := startRecv(stream)
	expectNoResult(t, pending, 150*time.Millisecond)

	fixture.store.Replace(fixture.snapshot("v2"))
	second := expectResult(t, pending)
	if second.GetVersionInfo() != "sha256:v2" {
		t.Fatalf("version_info = %q, want sha256:v2", second.GetVersionInfo())
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
		if code := recvCode(t, unknown); code != codes.PermissionDenied {
			t.Fatalf("unknown resource code = %v, want PermissionDenied", code)
		}
	})
}

func TestStreamIgnoresNodeIdentity(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.store.Replace(fixture.snapshot("v1"))

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
	if code := recvCode(t, stream); code != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", code)
	}
}

func TestStreamDoesNotSendAnEmptyResponseOnMiss(t *testing.T) {
	fixture := newStreamFixture(t)
	stream := fixture.open(t, validToken, fixture.resourceName)

	pending := startRecv(stream)
	expectNoResult(t, pending, 200*time.Millisecond)

	fixture.store.Replace(fixture.snapshot("v1"))
	response := expectResult(t, pending)
	if response.GetVersionInfo() != "sha256:v1" {
		t.Fatalf("version_info = %q, want sha256:v1", response.GetVersionInfo())
	}
}

func TestStreamRevokedWhenAuthorizationIsRemoved(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.store.Replace(fixture.snapshot("v1"))

	stream := fixture.open(t, validToken, fixture.resourceName)
	_ = recv(t, stream)

	fixture.authz.RevokeAndNotify(fixture.resourceName)
	if code := recvCode(t, stream); code != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", code)
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

func expectNoResult(t *testing.T, results <-chan recvResult, wait time.Duration) {
	t.Helper()
	select {
	case result := <-results:
		t.Fatalf("Recv() = %+v, %v; want no response", result.response, result.err)
	case <-time.After(wait):
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
