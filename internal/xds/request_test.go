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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"k8s.io/apimachinery/pkg/types"
)

func (f *streamFixture) openRequest(t *testing.T, req *discoveryv3.DiscoveryRequest) (context.CancelFunc, func() (*discoveryv3.DiscoveryResponse, error)) {
	t.Helper()
	return f.openRequestAs(t, validToken, req)
}

func (f *streamFixture) openRequestAs(t *testing.T, token string, req *discoveryv3.DiscoveryRequest) (context.CancelFunc, func() (*discoveryv3.DiscoveryResponse, error)) {
	t.Helper()
	ctx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token))
	stream, err := f.client.StreamExtensionConfigs(ctx)
	require.NoError(t, err)
	send(t, stream, req)
	return cancel, stream.Recv
}

// watches returns the cache watches held for a node.
func (f *streamFixture) watches(node string) int {
	if info := f.store.cache.GetStatusInfo(node); info != nil {
		return info.GetNumWatches()
	}
	return 0
}

func TestStreamRefusesForeignTypeURLs(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	const requests = 100
	for i := range requests {
		cancel, recvNext := fixture.openRequest(t, &discoveryv3.DiscoveryRequest{
			TypeUrl:       fmt.Sprintf("type.googleapis.com/example.Type%d", i),
			ResourceNames: []string{fixture.resourceName},
		})
		_, err := recvNext()
		require.Equal(t, codes.InvalidArgument, status.Code(err), "request %d", i)
		cancel()
	}

	require.Zero(t, fixture.watches(""), "requests with a foreign type URL must not retain watches")
	require.Zero(t, fixture.watches(testNamespace+"/"+streamEngine))
	require.Equal(t, float64(requests), testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalInvalidRequest)))
	fixture.requireNoStreams(t)
}

// An ACK of the current response never makes the cache resend it, whatever
// version_info it claims, and a real publication still reaches the stream.
func TestStreamAckWithForeignVersionSendsNothing(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)

	for i := range 50 {
		send(t, stream, &discoveryv3.DiscoveryRequest{
			ResourceNames: []string{fixture.resourceName},
			VersionInfo:   fmt.Sprintf("sha256:foreign-%d", i),
			ResponseNonce: first.GetNonce(),
		})
	}
	updates := startRecv(stream)
	expectNoResult(t, updates)

	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	require.Equal(t, "sha256:v2", expectResult(t, updates).GetVersionInfo())
}

// An identity authorized for nothing is refused before it takes a registry
// slot, on subscriptions and on rejections alike.
func TestStreamUnauthorizedIdentityHoldsNoSlots(t *testing.T) {
	fixture := newStreamFixture(t)
	other := ResourceName(testNamespace, "other", types.UID("other-uid"))
	for i := range 20 {
		req := &discoveryv3.DiscoveryRequest{ResourceNames: []string{other}}
		if i%2 == 1 {
			req.ErrorDetail = status.New(codes.InvalidArgument, "rejected by Envoy").Proto()
		}
		cancel, recvNext := fixture.openRequest(t, req)
		_, err := recvNext()
		require.Equal(t, codes.Unavailable, status.Code(err), "request %d", i)
		cancel()
	}
	require.Zero(t, testutil.ToFloat64(fixture.metrics.streamReconnects), "no stream may have been registered")
	fixture.requireNoStreams(t)
}

// Fresh streams of one pod get a bounded number of full responses; the rest
// are refused with a retryable status.
func TestStreamResponseBudgetBoundsFreshStreams(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	const streams = 200
	responses := 0
	started := time.Now()
	for i := range streams {
		cancel, recvNext := fixture.openRequestAs(t, podToken("pod-a"), &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}})
		if _, err := recvNext(); err == nil {
			responses++
		} else {
			require.Equal(t, codes.ResourceExhausted, status.Code(err), "stream %d", i)
		}
		cancel()
		// Keep the pod under its stream cap so only the budget refuses.
		fixture.requireNoStreams(t)
	}
	elapsed := time.Since(started)
	allowed := responseBudgetBurst + int(float64(responseBudgetRate)*elapsed.Seconds()) + 1
	t.Logf("%d fresh streams in %v: %d full responses (allowed at most %d)", streams, elapsed, responses, allowed)
	require.GreaterOrEqual(t, responses, responseBudgetBurst)
	require.LessOrEqual(t, responses, allowed)
	require.Equal(t, float64(streams-responses), testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalResponseBudget)))
}

// A rejection sent as the first message registers the stream for revocation.
func TestStreamNackBeforeSubscriptionIsRevocable(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	cancel, recvNext := fixture.openRequest(t, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName},
		ErrorDetail:   status.New(codes.InvalidArgument, strings.Repeat("rejected by Envoy ", 4096)).Proto(),
	})
	defer cancel()
	require.Eventually(t, func() bool { return fixture.registeredStreams() == 1 }, 5*time.Second, 10*time.Millisecond)

	fixture.authz.RevokeAndNotify(fixture.resourceName)
	_, err := recvNext()
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestStreamRefusesOversizedFields(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	for name, req := range map[string]*discoveryv3.DiscoveryRequest{
		"version_info":   {ResourceNames: []string{fixture.resourceName}, VersionInfo: strings.Repeat("v", maxVersionInfoBytes+1)},
		"response_nonce": {ResourceNames: []string{fixture.resourceName}, ResponseNonce: strings.Repeat("1", maxNonceBytes+1)},
		"resource_name":  {ResourceNames: []string{resourceNamePrefix + strings.Repeat("a", maxResourceNameBytes)}},
	} {
		cancel, recvNext := fixture.openRequest(t, req)
		_, err := recvNext()
		require.Equal(t, codes.InvalidArgument, status.Code(err), name)
		cancel()
	}
}

// Every request is authorized, not only the one that subscribed.
func TestStreamAuthorizesEveryRequest(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	fixture.awaitWritten(t)

	// Drop the mapping without notifying live streams.
	fixture.authz.mu.Lock()
	delete(fixture.authz.entries, fixture.resourceName)
	fixture.authz.mu.Unlock()

	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: first.GetVersionInfo(), ResponseNonce: first.GetNonce(),
	})
	require.Equal(t, codes.Unavailable, recvCode(t, stream))
}

// The stream keeps only the pinned node ID, not the client's node metadata.
func TestStreamKeepsOnlyPinnedNode(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	nodeMetadata, err := structpb.NewStruct(map[string]any{"labels": strings.Repeat("x", 40<<10)})
	require.NoError(t, err)

	cancel, recvNext := fixture.openRequest(t, &discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "router~10.0.0.1~gateway-0.shop~shop.svc.cluster.local", Cluster: "gateway", Metadata: nodeMetadata},
		ResourceNames: []string{fixture.resourceName},
	})
	defer cancel()
	_, err = recvNext()
	require.NoError(t, err)

	node := fixture.store.cache.GetStatusInfo(testNamespace + "/" + streamEngine).GetNode()
	require.Equal(t, testNamespace+"/"+streamEngine, node.GetId())
	require.Nil(t, node.GetMetadata())
	require.Empty(t, node.GetCluster())
}

// A request the cache answers in full is charged whatever nonce it carries:
// after an ACK, requests with an unknown nonce and a foreign version are
// refused once the budget is spent.
func TestStreamResponseBudgetChargesForeignVersions(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	share, _, _ := fixture.authz.podShare(fixture.identity)
	started := time.Now()
	stream := fixture.open(t, validToken, fixture.resourceName)
	response := recv(t, stream)
	fixture.awaitWritten(t)

	responses := 1
	var err error
	for err == nil && responses <= 2*responseBudgetBurst*share {
		send(t, stream, &discoveryv3.DiscoveryRequest{
			ResourceNames: []string{fixture.resourceName}, VersionInfo: response.GetVersionInfo(), ResponseNonce: response.GetNonce(),
		})
		send(t, stream, &discoveryv3.DiscoveryRequest{VersionInfo: "x", ResponseNonce: "bogus"})
		if response, err = stream.Recv(); err == nil {
			responses++
			fixture.awaitWritten(t)
		}
	}
	// The bucket refills while the loop runs.
	allowed := responseBudgetBurst*share + int(float64(responseBudgetRate*share)*time.Since(started).Seconds()) + 1
	t.Logf("ACK and foreign-version pairs got %d full responses before %v (allowed at most %d)", responses, status.Code(err), allowed)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.LessOrEqual(t, responses, allowed)
	require.Equal(t, float64(1), testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalResponseBudget)))
}

// Refused subscriptions are logged only at V(1), so a peer cannot write a
// default-level log line per request.
func TestStreamUnauthorizedSubscriptionsAreNotLoggedByDefault(t *testing.T) {
	fixture := newStreamFixture(t)
	var lines atomic.Int64
	// funcr writes only lines at the default verbosity.
	fixture.server.Logger = funcr.New(func(_, _ string) { lines.Add(1) }, funcr.Options{})
	other := ResourceName(testNamespace, "other", types.UID("other-uid"))
	for range 20 {
		cancel, recvNext := fixture.openRequest(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{other}})
		_, err := recvNext()
		require.Equal(t, codes.Unavailable, status.Code(err))
		cancel()
	}
	require.Zero(t, lines.Load())
}

// A proxy repeating its acknowledgement logs it once per version.
func TestStreamRepeatedAcknowledgementIsLoggedOnce(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	var lines atomic.Int64
	fixture.server.Logger = funcr.New(func(_, _ string) { lines.Add(1) }, funcr.Options{})
	stream := fixture.open(t, validToken, fixture.resourceName)
	response := recv(t, stream)
	ack := &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: response.GetVersionInfo(), ResponseNonce: response.GetNonce(),
	}
	for range 500 {
		send(t, stream, ack)
	}
	updates := startRecv(stream)
	expectNoResult(t, updates)
	require.Equal(t, int64(1), lines.Load())

	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	next := expectResult(t, updates)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: next.GetVersionInfo(), ResponseNonce: next.GetNonce(),
	})
	require.Eventually(t, func() bool { return lines.Load() == 2 }, 5*time.Second, 10*time.Millisecond)
}

// A proxy whose first message rejects the configuration is resubscribed when
// a newer snapshot supersedes the retry, and then receives that snapshot.
func TestStreamFirstNackRetrySupersededByNewerSnapshot(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 300 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken), 5*time.Second)
	defer cancel()
	stream, err := fixture.client.StreamExtensionConfigs(ctx)
	require.NoError(t, err)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: "proxy-nack-first"},
		ResourceNames: []string{fixture.resourceName},
		ResponseNonce: "7",
		ErrorDetail:   status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	})
	require.Eventually(t, func() bool { return fixture.registeredStreams() == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	require.Equal(t, codes.Unavailable, recvCode(t, stream), "the stream must be resubscribed")

	resubscribed := fixture.open(t, validToken, fixture.resourceName)
	require.Equal(t, "sha256:v2", recv(t, resubscribed).GetVersionInfo())
}

// A NACK that names no resource is resolved to the one fresh grant, ignoring
// a grant past its staleness bound; several fresh grants stay ambiguous.
func TestStreamNamelessNackIgnoresStaleGrants(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stale := fixture.authz.entries[fixture.resourceName]
	stale.ResourceName = ResourceName(testNamespace, "old-engine", "old-uid")
	stale.EngineName = "old-engine"
	stale.RefreshedAt = time.Now().Add(-time.Hour)
	fixture.authz.entries[stale.ResourceName] = stale
	require.Equal(t, fixture.resourceName, fixture.authz.soleResource(fixture.identity))

	cancel, _ := fixture.openRequest(t, &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "p"}, ResponseNonce: "3",
		ErrorDetail: status.New(codes.InvalidArgument, "rejected by Envoy").Proto()})
	defer cancel()
	require.Eventually(t, func() bool { return fixture.registeredStreams() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the NACK must be resolved to the fresh grant and its retry scheduled")

	fresh := stale
	fresh.RefreshedAt = time.Now()
	fixture.authz.mu.Lock() // the stream is still served
	fixture.authz.entries[fresh.ResourceName] = fresh
	fixture.authz.mu.Unlock()
	require.Empty(t, fixture.authz.soleResource(fixture.identity), "two fresh grants are ambiguous")
}

// A stream that rejected a version, then accepted it once resent, keeps going
// when its retry runs after a newer snapshot was sent to it: that snapshot
// supersedes the retry.
func TestStreamNackRetrySupersededOnStreamSentNewerSnapshot(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 300 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken), 5*time.Second)
	defer cancel()
	stream, err := fixture.client.StreamExtensionConfigs(ctx)
	require.NoError(t, err)
	send(t, stream, &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "proxy-nack"}, ResourceNames: []string{fixture.resourceName}})
	first := recv(t, stream)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, ResponseNonce: first.GetNonce(),
		ErrorDetail: status.New(codes.InvalidArgument, "rejected by Envoy").Proto(),
	})
	// The rejected version is sent again, and this time accepted.
	resent := recv(t, stream)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: resent.GetVersionInfo(), ResponseNonce: resent.GetNonce(),
	})
	node := testNamespace + "/" + streamEngine
	require.Eventually(t, func() bool { return fixture.watches(node) == 1 }, 5*time.Second, time.Millisecond)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	require.Equal(t, "sha256:v2", recv(t, stream).GetVersionInfo())

	// Past the retry delay the stream is still open.
	received := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		received <- err
	}()
	select {
	case err := <-received:
		t.Fatalf("the stream ended or answered after its retry: %v", err)
	case <-time.After(3 * fixture.server.RetryDelay):
	}
}
