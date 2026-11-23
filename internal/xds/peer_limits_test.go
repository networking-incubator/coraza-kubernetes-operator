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
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
)

// observePeak raises peak to value.
func observePeak(peak *atomic.Int32, value int32) {
	for current := peak.Load(); value > current && !peak.CompareAndSwap(current, value); current = peak.Load() {
	}
}

// subscribe opens a stream with token and waits for its first response.
func (f *listenerFixture) subscribe(ctx context.Context, client extensionv3.ExtensionConfigDiscoveryServiceClient, token string) error {
	stream, err := client.StreamExtensionConfigs(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token))
	if err != nil {
		return err
	}
	// A refused stream may already be closed; Recv then returns its status.
	_ = stream.Send(&discoveryv3.DiscoveryRequest{ResourceNames: []string{f.resourceName}})
	_, err = stream.Recv()
	return err
}

func TestStreamCapsPerPod(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := range podStreamCap {
		err := fixture.subscribe(ctx, fixture.newECDSClient(t), podToken("pod-a"))
		require.NoError(t, err, "stream %d", i)
	}
	err := fixture.subscribe(ctx, fixture.newECDSClient(t), podToken("pod-a"))
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "a pod over its stream cap must be refused")
	err = fixture.subscribe(ctx, fixture.newECDSClient(t), podToken("pod-b"))
	require.NoError(t, err, "another pod of the same ServiceAccount must not be refused")
	require.Equal(t, float64(1), testutil.ToFloat64(fixture.metrics.streamLimitRejections))
}

// A Gateway with 33 pods sharing one ServiceAccount gets a stream for every
// pod, with pod-bound tokens and with ServiceAccount tokens alike.
func TestStreamGatewayPodsSharingServiceAccountAreNotRefused(t *testing.T) {
	const pods = 33
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	fixture.authz.entries[fixture.resourceName].Pods[fixture.identity.Key()] = pods
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := range pods {
		err := fixture.subscribe(ctx, fixture.newECDSClient(t), podToken(fmt.Sprintf("pod-%d", i)))
		require.NoError(t, err, "pod %d", i)
		err = fixture.subscribe(ctx, fixture.newECDSClient(t), validToken)
		require.NoError(t, err, "ServiceAccount token %d", i)
	}
	require.Zero(t, testutil.ToFloat64(fixture.metrics.streamLimitRejections))
}

// The authorization index counts a new pod only after a refresh. A
// ServiceAccount counted with one pod may still hold 4 plus 16 streams, so ten
// Envoys of a scale-up, one stream each, are not refused before the refresh.
func TestStreamIdentityCapToleratesScaleUp(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	fixture.authz.entries[fixture.resourceName].Pods[fixture.identity.Key()] = 1
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const pods = 10 // one counted, nine not yet refreshed
	for i := range pods {
		err := fixture.subscribe(ctx, fixture.newECDSClient(t), podToken(fmt.Sprintf("new-pod-%d", i)))
		require.NoError(t, err, "pod %d", i)
	}
	require.Zero(t, testutil.ToFloat64(fixture.metrics.streamLimitRejections))
}

// Identities that authenticate but that no Engine grants are refused at
// admission, so 128 of them reopening four streams each cannot keep a Gateway
// out of the stream slots.
func TestStreamIdentitiesGrantedNothingCannotFillSlots(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	fixture.server.Auth.tokenReviews = reviewFunc(func(_ context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		if name, ok := strings.CutPrefix(review.Spec.Token, "outsider:"); ok {
			return reviewFor(Identity{Namespace: "outsiders", Name: name, UID: "uid-" + name}, validToken, true), nil
		}
		return reviewFor(fixture.identity, review.Spec.Token, true), nil
	})

	const identities = 128
	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer func() { close(stop); wg.Wait() }()
	var clients []*rawH2
	for i := range identities * podStreamCap {
		if i%h2MaxConcurrentStreams == 0 {
			clients = append(clients, dialRawH2(t, fixture.listenerFixture, 65535))
		}
		client, token := clients[len(clients)-1], fmt.Sprintf("outsider:sa-%d", i/podStreamCap)
		wg.Go(func() {
			for {
				id := client.openGRPC(ecdsStreamMethod, token)
				select {
				case <-client.channel(id):
				case <-stop:
					return
				}
			}
		})
	}
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalUnauthorized)) >= identities*podStreamCap
	}, 10*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, fixture.subscribe(ctx, fixture.newECDSClient(t), podToken("gateway-pod")))
	t.Logf("%d outsider streams refused while a Gateway was admitted; open streams %d",
		int(testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalUnauthorized))), fixture.server.openStreams.Load())
	require.Equal(t, int64(1), fixture.server.openStreams.Load())
}

// A burst of 48 streams receiving a large configuration at full speed is not
// reset by the write timeout.
func TestStreamBurstOfLargeResponsesIsNotReset(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	limits.WriteTimeout = 10 * time.Second
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.largeSnapshot(t, "v1", 2<<20)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const streams = 48
	errs := make([]error, streams)
	var wg sync.WaitGroup
	started := time.Now()
	clients := make([]extensionv3.ExtensionConfigDiscoveryServiceClient, streams/h2MaxConcurrentStreams)
	for i := range clients {
		clients[i] = fixture.newECDSClient(t)
	}
	for i := range streams {
		client := clients[i/h2MaxConcurrentStreams]
		wg.Go(func() { errs[i] = fixture.subscribe(ctx, client, podToken(fmt.Sprintf("burst-%d", i))) })
	}
	wg.Wait()
	t.Logf("%d streams received a 2 MiB configuration in %v", streams, time.Since(started))
	for i, err := range errs {
		require.NoError(t, err, "stream %d", i)
	}
	require.Zero(t, testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalWriteTimeout)))
}

// An uncached token waits for an authentication slot instead of being
// refused, and is refused only once the wait exceeds its bound; a cached
// token does not wait behind the pool.
func TestAuthenticationPoolQueuesForBoundedTime(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStreams = authenticationShare // one authentication slot
	limits.TokenReviewTimeout = 300 * time.Millisecond
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	_, err := fixture.server.Auth.Authenticate(context.Background(), validToken)
	require.NoError(t, err)

	release := make(chan struct{})
	var inFlight, peak atomic.Int32
	fixture.server.Auth.tokenReviews = reviewFunc(func(ctx context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		observePeak(&peak, inFlight.Add(1))
		defer inFlight.Add(-1)
		select {
		case <-release:
		case <-time.After(100 * time.Millisecond):
		}
		return reviewFor(fixture.identity, review.Spec.Token, true), nil
	})

	// Two uncached tokens share the one slot: the second queues and succeeds.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		client := fixture.newECDSClient(t)
		wg.Go(func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errs[i] = fixture.subscribe(ctx, client, podToken(fmt.Sprintf("queued-%d", i)))
		})
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, int32(1), peak.Load(), "reviews must not exceed the pool")
	require.Zero(t, testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalAuthenticationQueue)))

	// A review that holds the slot past the wait bound gets the next one refused.
	fixture.server.Auth.tokenReviews = reviewFunc(func(ctx context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return reviewFor(fixture.identity, review.Spec.Token, true), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	holder := fixture.newECDSClient(t)
	go func() { _ = fixture.subscribe(ctx, holder, podToken("holder")) }()
	require.Eventually(t, func() bool { return len(fixture.server.authSlots) == 1 }, 5*time.Second, time.Millisecond)
	err = fixture.subscribe(ctx, fixture.newECDSClient(t), podToken("late"))
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Equal(t, float64(1), testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalAuthenticationQueue)))
	err = fixture.subscribe(ctx, fixture.newECDSClient(t), validToken)
	require.NoError(t, err, "a cached token must not wait behind pending reviews")
	close(release)
}

// After an operator restart every Gateway pod reconnects at once with a token
// the new replica has not cached. Simulating Envoy's reconnect back-off
// (jittered, 500ms doubling to 30s), all 500 pods of one ServiceAccount get
// their configuration with the default limits.
func TestStreamReconnectStormWithUncachedTokensConverges(t *testing.T) {
	const pods = 500
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	fixture.authz.entries[fixture.resourceName].Pods[fixture.identity.Key()] = pods
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	var reviews, inFlight, peak atomic.Int32
	fixture.server.Auth.tokenReviews = reviewFunc(func(ctx context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		reviews.Add(1)
		observePeak(&peak, inFlight.Add(1))
		defer inFlight.Add(-1)
		time.Sleep(20 * time.Millisecond) // API server latency
		return reviewFor(fixture.identity, review.Spec.Token, true), nil
	})

	// Every attempt and back-off ends at the reconnect cutoff, so a stalled
	// one cannot hang the test; admitted streams stay open until the count.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var retries atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, pods)
	started := time.Now()
	for i := range pods {
		client := fixture.newECDSClient(t)
		wg.Go(func() {
			backoff := 500 * time.Millisecond
			for {
				err := fixture.subscribe(ctx, client, podToken(fmt.Sprintf("pod-%d", i)))
				if err == nil {
					return
				}
				retries.Add(1)
				select {
				case <-ctx.Done():
					errs <- fmt.Errorf("pod %d: %w", i, err)
					return
				case <-time.After(time.Duration(rand.Int64N(int64(backoff)))): //nolint:gosec // test jitter
				}
				backoff = min(2*backoff, 30*time.Second)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	elapsed := time.Since(started)
	t.Logf("%d pods converged in %v: %d TokenReviews, peak %d concurrent (pool %d), %d retries",
		pods, elapsed, reviews.Load(), peak.Load(), cap(fixture.server.permits), retries.Load())
	require.Less(t, elapsed, 30*time.Second, "convergence must stay within Envoy's back-off ceiling")
	require.LessOrEqual(t, int(peak.Load()), cap(fixture.server.permits))
	require.Equal(t, int64(pods), fixture.server.openStreams.Load())
}

func (f *listenerFixture) streamsOf(identity, podUID string) (int, int) {
	f.server.countsMu.Lock()
	defer f.server.countsMu.Unlock()
	return f.server.perIdentity[identity], f.server.perPod[podUID]
}

// ServiceAccounts an Engine grants, with tokens that are not pod-bound, may
// each hold 4 streams per counted pod plus the slack, and all of a namespace's
// together 4 per pod counted there plus the slack once: 33 accounts with one
// pod each cannot fill the stream slots, and another Engine's Gateway still
// subscribes.
func TestStreamServiceAccountsWithoutPodTokensAreBounded(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	const accounts = 33
	tenant := ResourceName("tenant", "engine", "tenant-uid")
	entry := AuthzEntry{
		ResourceName: tenant, Namespace: "tenant", EngineName: "engine", RefreshedAt: time.Now(),
		Identities: map[string]string{}, Pods: map[string]int{},
	}
	for i := range accounts {
		entry.Identities[fmt.Sprintf("tenant/sa-%d", i)] = fmt.Sprintf("uid-sa-%d", i)
		entry.Pods[fmt.Sprintf("tenant/sa-%d", i)] = 1
	}
	fixture.authz.entries[tenant] = entry
	fixture.server.Auth.tokenReviews = reviewFunc(func(_ context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		if name, ok := strings.CutPrefix(review.Spec.Token, "sa:"); ok {
			return reviewFor(Identity{Namespace: "tenant", Name: name, UID: "uid-" + name}, validToken, true), nil
		}
		return reviewFor(fixture.identity, review.Spec.Token, true), nil
	})

	var client *rawH2
	opened := 0
	open := func(account, streams int) {
		for range streams {
			if opened%h2MaxConcurrentStreams == 0 {
				client = dialRawH2(t, fixture.listenerFixture, 65535)
			}
			client.openGRPC(ecdsStreamMethod, fmt.Sprintf("sa:sa-%d", account))
			opened++
		}
	}
	rejected := func() float64 { return testutil.ToFloat64(fixture.metrics.streamLimitRejections) }
	identityCap := podStreamCap*1 + identityStreamSlack
	namespaceCap := podStreamCap*accounts + identityStreamSlack

	// One account reaches its own cap and is refused one more stream.
	open(0, identityCap+1)
	require.Eventually(t, func() bool {
		first, _ := fixture.streamsOf("tenant/sa-0", "")
		return first == identityCap && rejected() == 1
	}, 5*time.Second, 10*time.Millisecond)

	// The others, each asking for more than its own cap, fill only the rest
	// of the namespace's allowance.
	const attempts = 24
	for account := 1; account < accounts; account++ {
		open(account, attempts)
	}
	require.Eventually(t, func() bool {
		return rejected() == float64(1+(accounts-1)*attempts-(namespaceCap-identityCap))
	}, 5*time.Second, 10*time.Millisecond)
	t.Logf("%d ServiceAccounts with one pod each in one namespace hold %d streams (namespace cap %d, %d per account)",
		accounts, fixture.server.openStreams.Load(), namespaceCap, identityCap)
	require.EqualValues(t, namespaceCap, fixture.server.openStreams.Load())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, fixture.subscribe(ctx, fixture.newECDSClient(t), podToken("gateway-pod")))
}

// A request that ends while its token is reviewed frees its authentication
// slot at once; the review keeps its permit until it finishes and still
// charges the rejected token.
func TestAuthenticationSlotIsFreedWhenRequestEnds(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStreams = authenticationShare // one authentication slot
	limits.AuthFailureRate = 0.001
	limits.AuthFailureBurst = 2
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	fixture.server.permits = make(chan struct{}, 2) // one for the reset request's review
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	release := make(chan struct{})
	// The review ignores cancellation, so release it however the test ends.
	releaseReview := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseReview)
	var reviews atomic.Int32
	fixture.server.Auth.tokenReviews = reviewFunc(func(_ context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		reviews.Add(1)
		if strings.HasPrefix(review.Spec.Token, "rejected") {
			<-release
			return rejectedReview(), nil
		}
		return reviewFor(fixture.identity, review.Spec.Token, true), nil
	})

	client := dialRawH2(t, fixture.listenerFixture, 65535)
	id := client.openGRPC(ecdsStreamMethod, "rejected-1")
	require.Eventually(t, func() bool { return reviews.Load() == 1 }, 5*time.Second, time.Millisecond)
	reset := time.Now()
	require.NoError(t, client.reset(id))

	// The review of the reset request is still running and keeps its permit,
	// yet a Gateway gets the one slot.
	require.Eventually(t, func() bool { return len(fixture.server.authSlots) == 0 }, 5*time.Second, time.Millisecond)
	require.Len(t, fixture.server.permits, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, fixture.subscribe(ctx, fixture.newECDSClient(t), podToken("gateway-pod")))
	t.Logf("a Gateway was admitted %v after a request under review was reset", time.Since(reset))

	releaseReview()
	require.Eventually(t, func() bool { return len(fixture.server.permits) == 0 }, 5*time.Second, time.Millisecond)
	require.Less(t, fixture.settledFailureTokens(t), float64(limits.AuthFailureBurst)-0.5, "the reset request's rejected token must have been charged")
}

// Peers on many source addresses that reset their requests while the tokens
// are reviewed cannot run more TokenReviews at once than the review cap, and a
// Gateway is still admitted.
func TestTokenReviewsStayCappedWhenRequestsReset(t *testing.T) {
	sources := make([]string, 0, 8)
	for i := 2; i < 10; i++ {
		source := fmt.Sprintf("127.0.0.%d", i)
		requireLocalAddress(t, source)
		sources = append(sources, source)
	}
	limits := DefaultLimits()
	limits.MaxStreams = 32 // 8 authentication slots and 8 review permits
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	reviewCap := int32(cap(fixture.server.permits))
	var inFlight, peak atomic.Int32
	fixture.server.Auth.tokenReviews = reviewFunc(func(_ context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		observePeak(&peak, inFlight.Add(1))
		defer inFlight.Add(-1)
		if strings.HasPrefix(review.Spec.Token, "rejected") {
			time.Sleep(100 * time.Millisecond)
			return rejectedReview(), nil
		}
		return reviewFor(fixture.identity, review.Spec.Token, true), nil
	})

	// Each source keeps six requests open at a time, resetting each 10ms in.
	// The workers stop however the test ends, before their connections close.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	stopFlood := sync.OnceFunc(func() { close(stop); wg.Wait() })
	defer stopFlood()
	var opened atomic.Int64
	for _, source := range sources {
		client := dialRawH2From(t, fixture.listenerFixture, source, 65535)
		for range h2MaxConcurrentStreams - 2 {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					id := client.openGRPC(ecdsStreamMethod, fmt.Sprintf("rejected-%d", opened.Add(1)))
					time.Sleep(10 * time.Millisecond)
					_ = client.reset(id)
				}
			})
		}
	}
	require.Eventually(t, func() bool { return peak.Load() >= reviewCap }, 5*time.Second, time.Millisecond)

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), limits.WithDefaults().TokenReviewTimeout)
	defer cancel()
	require.NoError(t, fixture.subscribe(ctx, fixture.newECDSClient(t), podToken("gateway-pod")))
	admitted := time.Since(started)
	stopFlood()
	require.Eventually(t, func() bool { return inFlight.Load() == 0 }, 5*time.Second, time.Millisecond)
	t.Logf("%d sources reset %d requests under review: peak %d TokenReviews in flight (cap %d); a Gateway was admitted in %v",
		len(sources), opened.Load(), peak.Load(), reviewCap, admitted)
	require.LessOrEqual(t, peak.Load(), reviewCap)
}

// A pod that keeps opening streams and ending them while their responses are
// still in flight holds at most podStreamCap of them at a time, and the
// responses it leaves behind do not pile up in memory.
func TestStreamPodCannotKeepResponsesInFlight(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	const size = 8 << 20
	require.NoError(t, fixture.store.Replace(fixture.largeSnapshot(t, "v1", size)))
	subscription := grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}})
	invalid := grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName, fixture.resourceName + "-other"}})

	before := heapBytes()
	most := 0
	var client *rawH2
	var writing int64
	for i := range 16 {
		if i%(h2MaxConcurrentStreams-1) == 0 {
			client = dialRawH2(t, fixture.listenerFixture, 0) // responses can never be sent
		}
		id := client.openGRPC(ecdsStreamMethod, podToken("pod-z"))
		client.data(id, subscription, h2MaxReadFrameSize)
		// The stream's response is being written before its handler ends.
		require.Eventually(t, func() bool { return fixture.responseWriting() > writing }, 5*time.Second, time.Millisecond)
		writing = fixture.responseWriting()
		_, inFlight := fixture.streamsOf(fixture.identity.Key(), "pod-z")
		client.data(id, invalid, h2MaxReadFrameSize)
		time.Sleep(30 * time.Millisecond)
		_, left := fixture.streamsOf(fixture.identity.Key(), "pod-z")
		most = max(most, inFlight, left)
	}
	held := heapGrowth(before, heapBytes())
	t.Logf("a pod opening and ending 16 streams with %d MiB responses in flight held at most %d streams; heap growth %d MiB",
		size>>20, most, held>>20)
	require.LessOrEqual(t, most, podStreamCap)
	require.Less(t, held, uint64((podStreamCap+1)*size+(16<<20)))
}

// A token found in the cache is admitted with that identity: a lookup that
// expires right after it does not lead to a review, and a review only runs
// holding an authentication slot and a review permit.
func TestAuthenticationUsesCachedIdentityAsFound(t *testing.T) {
	fixture := newStreamFixture(t)
	auth := fixture.server.Auth
	_, err := auth.Authenticate(context.Background(), validToken)
	require.NoError(t, err)
	var reviews, slots, permits atomic.Int32
	auth.tokenReviews = reviewFunc(func(_ context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		reviews.Add(1)
		slots.Store(int32(len(fixture.server.authSlots)))
		permits.Store(int32(len(fixture.server.permits)))
		return reviewFor(fixture.identity, review.Spec.Token, true), nil
	})
	// The cached identity expires right after the first lookup.
	var lookups atomic.Int32
	auth.now = func() time.Time {
		if lookups.Add(1) == 1 {
			return time.Now()
		}
		return time.Now().Add(48 * time.Hour)
	}
	noReview := func() func() { return func() {} }

	identity, err := fixture.server.authenticate(httptest.NewRequest(http.MethodPost, "/", nil), validToken, noReview)
	require.NoError(t, err)
	require.Equal(t, fixture.identity.Key(), identity.Key())
	require.Zero(t, reviews.Load(), "a cached token must not be reviewed")

	_, err = fixture.server.authenticate(httptest.NewRequest(http.MethodPost, "/", nil), validToken, noReview)
	require.NoError(t, err)
	require.EqualValues(t, 1, reviews.Load())
	require.EqualValues(t, 1, slots.Load(), "the review must hold an authentication slot")
	require.EqualValues(t, 1, permits.Load(), "the review must hold a review permit")
}

// A source whose failure budget is spent is refused before it can take an
// authentication slot or a review permit, so it neither waits for them nor
// holds them from other sources.
func TestAuthenticationThrottledSourceTakesNoSlot(t *testing.T) {
	requireLocalAddress(t, "127.0.0.2")
	limits := DefaultLimits()
	limits.MaxStreams = authenticationShare // one slot and one permit
	limits.AuthFailureRate = 0.001
	limits.AuthFailureBurst = 1
	limits.TokenReviewTimeout = time.Second
	fixture := newStreamFixtureWithLimits(t, limits)
	release := make(chan struct{})
	releaseReview := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseReview)
	var reviews atomic.Int32
	fixture.server.Auth.tokenReviews = reviewFunc(func(_ context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		reviews.Add(1)
		if review.Spec.Token == "held" {
			<-release
			return reviewFor(fixture.identity, review.Spec.Token, true), nil
		}
		return rejectedReview(), nil
	})

	// One rejected token spends the first source's budget.
	client := dialRawH2(t, fixture.listenerFixture, 65535)
	require.Equal(t, strconv.Itoa(int(codes.Unauthenticated)), client.wait(client.openGRPC(ecdsStreamMethod, "rejected-1"), 5*time.Second).grpcStatus)
	// Another source holds the slot and the permit with a review in flight.
	dialRawH2From(t, fixture.listenerFixture, "127.0.0.2", 65535).openGRPC(ecdsStreamMethod, "held")
	require.Eventually(t, func() bool {
		return len(fixture.server.authSlots) == 1 && len(fixture.server.permits) == 1
	}, 5*time.Second, time.Millisecond)

	started := time.Now()
	result := client.wait(client.openGRPC(ecdsStreamMethod, "rejected-2"), 5*time.Second)
	t.Logf("a throttled source was refused after %v", time.Since(started))
	require.Equal(t, strconv.Itoa(int(codes.ResourceExhausted)), result.grpcStatus)
	require.Less(t, time.Since(started), limits.TokenReviewTimeout/2, "a throttled source must not wait for a slot")
	require.Equal(t, float64(1), testutil.ToFloat64(fixture.metrics.authThrottled))
	require.Zero(t, testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalAuthenticationQueue)))
	require.EqualValues(t, 2, reviews.Load())
}

// A request waiting for an authentication slot holds none of its source's
// failure budget, so valid tokens queued behind a review are not throttled.
func TestAuthenticationQueuedTokensSpendNoFailureBudget(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStreams = authenticationShare // one slot and one permit
	limits.AuthFailureBurst = 2
	fixture := newStreamFixtureWithLimits(t, limits)
	fixture.authz.entries[fixture.resourceName].Pods[fixture.identity.Key()] = 3
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	release := make(chan struct{})
	releaseReview := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseReview)
	fixture.server.Auth.tokenReviews = reviewFunc(func(_ context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		<-release
		return reviewFor(fixture.identity, review.Spec.Token, true), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errs := make(chan error, 3)
	for i := range 3 {
		client := fixture.newECDSClient(t)
		go func() { errs <- fixture.subscribe(ctx, client, podToken(fmt.Sprintf("pod-%d", i))) }()
	}
	// One review is in flight; the other two requests wait for the slot.
	require.Eventually(t, func() bool { return len(fixture.server.permits) == 1 }, 5*time.Second, time.Millisecond)
	require.Never(t, func() bool { return testutil.ToFloat64(fixture.metrics.authThrottled) > 0 },
		200*time.Millisecond, 10*time.Millisecond, "a valid token waiting for a slot must not be throttled")
	releaseReview()
	for range 3 {
		require.NoError(t, <-errs)
	}
}
