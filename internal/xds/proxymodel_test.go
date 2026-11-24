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
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	wasmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// This file models how an Envoy proxy consumes one ECDS resource, so tests can
// follow a Gateway that rejects a configuration until it applies one. The model
// follows Envoy's extension config subscription and its gRPC mux (read from
// Envoy 1.37 and 1.40; the unified mux, off by default, is not modelled):
//
//   - The subscription hashes the typed config of every response and stores the
//     hash before it builds the filter. Bytes with the stored hash are
//     accepted without a build; bytes with another hash are built. A failed
//     build rejects the response but leaves the hash stored, so the same
//     bytes are never built again.
//   - A Wasm filter that sets nack_on_code_cache_miss fails its build while its
//     remote module is not in Envoy's code cache, and starts a background
//     fetch. Builds keep failing until the fetch ends.
//   - The mux keeps one request: the version it last accepted, the nonce of the
//     last response, and the error detail of a rejection until it is sent. A
//     rejection keeps the last accepted version. A stream starts with that
//     request with the nonce cleared, so it reports the last accepted version
//     and replays a rejection that was never sent.

const (
	// modelResourceName is the resource the client unit tests subscribe to.
	modelResourceName = "coraza.waf.ns.engine.uid"
	// neverFetched makes every fetch of a module end without it.
	neverFetched = math.MaxInt
)

// errCodeNotCached is why a build that needs a module Envoy has not cached fails.
var errCodeNotCached = errors.New("remote Wasm code is not cached")

// proxyStats counts what a modelled proxy did.
type proxyStats struct {
	responses     int // responses processed
	builds        int // filter builds attempted
	buildFailures int // builds that failed
	skipped       int // responses accepted without a build because their bytes were the last received
	unbuilt       int // skipped responses whose bytes were never built
	acks          int // requests sent that accept a response
	nacks         int // requests sent that carry an error
}

// ecdsClient is the state of one ECDS subscription and its request. Like
// Envoy's main thread it is single-threaded; callers serialize access.
type ecdsClient struct {
	resourceName string
	node         string
	// build creates the filter from a config.
	build func(*wasmhttp.Wasm) error

	// The subscription keeps the hash of the typed config it received last,
	// built or not; the filter keeps the config it built, and that config's hash.
	lastHash    [sha256.Size]byte
	appliedHash [sha256.Size]byte
	applied     *wasmhttp.Wasm
	// The request keeps the last accepted version, the nonce of the last
	// response, the rejection not sent yet, and whether the next request opens
	// a stream.
	version   string
	nonce     string
	rejection error
	opening   bool

	// received lists the version of every response, in order.
	received []string
	stats    proxyStats
}

// streamEstablished starts a stream: the nonce is cleared, and the version and
// an unsent rejection are kept.
func (c *ecdsClient) streamEstablished() {
	c.nonce, c.opening = "", true
}

// onResponse processes one response the way the subscription does.
func (c *ecdsClient) onResponse(resp *discoveryv3.DiscoveryResponse) {
	c.stats.responses++
	c.received = append(c.received, resp.GetVersionInfo())
	if err := c.apply(resp); err != nil {
		c.rejection = err
	} else {
		c.version = resp.GetVersionInfo()
	}
	c.nonce = resp.GetNonce()
}

func (c *ecdsClient) apply(resp *discoveryv3.DiscoveryResponse) error {
	if len(resp.GetResources()) != 1 {
		return fmt.Errorf("unexpected number of resources in the response: %d", len(resp.GetResources()))
	}
	var typed corev3.TypedExtensionConfig
	if err := resp.GetResources()[0].UnmarshalTo(&typed); err != nil {
		return err
	}
	if typed.GetName() != c.resourceName {
		return fmt.Errorf("unexpected resource name in the response: %q", typed.GetName())
	}
	bytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(typed.GetTypedConfig())
	if err != nil {
		return err
	}
	hash := sha256.Sum256(bytes)
	if hash == c.lastHash {
		c.stats.skipped++
		if hash != c.appliedHash {
			c.stats.unbuilt++
		}
		return nil
	}
	c.lastHash = hash
	var config wasmhttp.Wasm
	if err := typed.GetTypedConfig().UnmarshalTo(&config); err != nil {
		return err
	}
	c.stats.builds++
	if err := c.build(&config); err != nil {
		c.stats.buildFailures++
		return err
	}
	c.applied, c.appliedHash = &config, hash
	return nil
}

// request returns the request to send now. The node is sent on the first
// request of a stream only, as Istio's bootstrap configures it.
func (c *ecdsClient) request() *discoveryv3.DiscoveryRequest {
	req := &discoveryv3.DiscoveryRequest{
		TypeUrl:       resource.ExtensionConfigType,
		ResourceNames: []string{c.resourceName},
		VersionInfo:   c.version,
		ResponseNonce: c.nonce,
	}
	if c.rejection != nil {
		req.ErrorDetail = status.New(codes.Internal, c.rejection.Error()).Proto()
	}
	if c.opening {
		req.Node = &corev3.Node{Id: c.node}
	}
	return req
}

// sent records that the request was handed to the stream. A rejection is
// forgotten from then on, whether or not the server processed it.
func (c *ecdsClient) sent() {
	switch {
	case c.rejection != nil:
		c.stats.nacks++
	case c.nonce != "":
		c.stats.acks++
	}
	c.rejection, c.opening = nil, false
}

// wasmModule is the remote module of a filter, and Envoy's code cache for it.
// Set its fields before the proxy starts.
type wasmModule struct {
	// fetchTime is how long a fetch takes. The first failFetches fetches end
	// without the module, so the build that follows starts another.
	fetchTime   time.Duration
	failFetches int

	mu       sync.Mutex
	cached   bool
	fetching bool
	fetches  int // fetches started
}

func (m *wasmModule) isCached() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cached
}

// build fails while the module is not cached, and the first failure starts the
// fetch. Only a config that sets nack_on_code_cache_miss needs the module.
func (m *wasmModule) build(config *wasmhttp.Wasm) error {
	if !config.GetConfig().GetVmConfig().GetNackOnCodeCacheMiss() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cached {
		return nil
	}
	if !m.fetching {
		m.fetching = true
		m.fetches++
		fails := m.fetches <= m.failFetches
		time.AfterFunc(m.fetchTime, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.fetching, m.cached = false, !fails
		})
	}
	return errCodeNotCached
}

// sendFate is what becomes of a request a proxy sends.
type sendFate int

const (
	// deliver sends the request.
	deliver sendFate = iota
	// lose ends the stream after the proxy sent the request: the proxy took it
	// for sent, and the server never reads it.
	lose
	// refuse ends the stream before the request was sent: a rejection stays
	// unsent, and the next stream replays it.
	refuse
)

// proxyModel is a proxy that keeps a stream to a test server open, and opens a
// new one when it ends. Give each proxy its own connection, as each Gateway pod
// has one: a server limits the streams of a connection.
type proxyModel struct {
	reconnect time.Duration
	module    *wasmModule
	// beforeSend, when set, decides the fate of every request. Set it before
	// the proxy starts.
	beforeSend func(*discoveryv3.DiscoveryRequest) sendFate

	mu     sync.Mutex
	rpc    extensionv3.ExtensionConfigDiscoveryServiceClient
	cancel context.CancelFunc // ends the current stream
	client ecdsClient
}

// newProxyModel returns a proxy of resourceName that connects with rpc and the
// fixture token. Start it once its module is set up.
func newProxyModel(resourceName, node string, rpc extensionv3.ExtensionConfigDiscoveryServiceClient) *proxyModel {
	p := &proxyModel{rpc: rpc, reconnect: 10 * time.Millisecond, module: &wasmModule{}}
	p.client = ecdsClient{resourceName: resourceName, node: node, build: p.module.build}
	return p
}

// start runs the proxy until the test ends.
func (p *proxyModel) start(t *testing.T) {
	var wg sync.WaitGroup
	wg.Go(func() { p.run(t.Context()) })
	t.Cleanup(wg.Wait)
}

// connectTo makes the proxy's next stream go to another server, as when it
// moves to another replica or the operator restarts. A stream that is open
// stays; see dropStream.
func (p *proxyModel) connectTo(rpc extensionv3.ExtensionConfigDiscoveryServiceClient) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rpc = rpc
}

// dropStream ends the proxy's stream from its side.
func (p *proxyModel) dropStream() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *proxyModel) run(ctx context.Context) {
	for ctx.Err() == nil {
		p.session(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(p.reconnect):
		}
	}
}

// session is one stream: it sends a request, processes the response to it and
// sends the next, until the stream ends.
func (p *proxyModel) session(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.mu.Lock()
	p.cancel = cancel
	rpc := p.rpc
	p.mu.Unlock()
	stream, err := rpc.StreamExtensionConfigs(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+validToken))
	if err != nil {
		return
	}
	p.mu.Lock()
	p.client.streamEstablished()
	req := p.client.request()
	p.mu.Unlock()
	for {
		fate := deliver
		if p.beforeSend != nil {
			fate = p.beforeSend(req)
		}
		if fate == refuse {
			return
		}
		// A request the stream refuses is not sent, so a rejection stays unsent.
		if fate == deliver && stream.Send(req) != nil {
			return
		}
		p.mu.Lock()
		p.client.sent()
		p.mu.Unlock()
		if fate == lose {
			return
		}
		resp, err := stream.Recv()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.client.onResponse(resp)
		req = p.client.request()
		p.mu.Unlock()
	}
}

// state returns a copy of the proxy's client: what it did, the version it
// reports as accepted, and the filter it built.
func (p *proxyModel) state() ecdsClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client
}

// ecdsResponse is the response that carries a Wasm filter config with vmID.
func ecdsResponse(t *testing.T, name, version, nonce, vmID string) *discoveryv3.DiscoveryResponse {
	t.Helper()
	typed, err := anypb.New(&corev3.TypedExtensionConfig{Name: name, TypedConfig: mustWasm(t, vmID)})
	require.NoError(t, err)
	return &discoveryv3.DiscoveryResponse{
		TypeUrl:     resource.ExtensionConfigType,
		VersionInfo: version,
		Nonce:       nonce,
		Resources:   []*anypb.Any{typed},
	}
}

func TestECDSClientProcessesResponses(t *testing.T) {
	type response struct {
		resource      string // overrides the subscribed resource's name
		version, vmID string
		buildErr      error
	}
	tests := []struct {
		name          string
		responses     []response
		wantVersion   string // the version the next request reports
		wantRejection bool
		wantBuilds    int
		wantSkipped   int
		wantUnbuilt   int
		wantApplied   string // VM ID of the built filter, empty when there is none
	}{
		{
			name:        "new bytes are built and accepted",
			responses:   []response{{version: "v1", vmID: "a"}},
			wantVersion: "v1", wantBuilds: 1, wantApplied: "a",
		},
		{
			name:        "identical bytes are accepted without a build",
			responses:   []response{{version: "v1", vmID: "a"}, {version: "v2", vmID: "a"}},
			wantVersion: "v2", wantBuilds: 1, wantSkipped: 1, wantApplied: "a",
		},
		{
			name:        "different bytes are built",
			responses:   []response{{version: "v1", vmID: "a"}, {version: "v2", vmID: "b"}},
			wantVersion: "v2", wantBuilds: 2, wantApplied: "b",
		},
		{
			name:          "a failed build is rejected at the last accepted version",
			responses:     []response{{version: "v1", vmID: "a"}, {version: "v2", vmID: "b", buildErr: errCodeNotCached}},
			wantVersion:   "v1",
			wantRejection: true, wantBuilds: 2, wantApplied: "a",
		},
		{
			name:          "a first response that fails is rejected without a version",
			responses:     []response{{version: "v1", vmID: "a", buildErr: errCodeNotCached}},
			wantRejection: true, wantBuilds: 1,
		},
		{
			name:        "rejected bytes sent again are accepted without being built",
			responses:   []response{{version: "v1", vmID: "a", buildErr: errCodeNotCached}, {version: "v1", vmID: "a"}},
			wantVersion: "v1", wantBuilds: 1, wantSkipped: 1, wantUnbuilt: 1,
		},
		{
			name:        "different bytes are built after a rejection",
			responses:   []response{{version: "v1", vmID: "a", buildErr: errCodeNotCached}, {version: "v2", vmID: "b"}},
			wantVersion: "v2", wantBuilds: 2, wantApplied: "b",
		},
		{
			name:          "a response for another resource is rejected",
			responses:     []response{{resource: "coraza.waf.ns.other.uid", version: "v1", vmID: "a"}},
			wantRejection: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buildErr error
			client := &ecdsClient{resourceName: modelResourceName, build: func(*wasmhttp.Wasm) error { return buildErr }}
			var req *discoveryv3.DiscoveryRequest
			for i, r := range tt.responses {
				buildErr = r.buildErr
				client.onResponse(ecdsResponse(t, cmp.Or(r.resource, modelResourceName), r.version, strconv.Itoa(i+1), r.vmID))
				req = client.request()
				client.sent()
			}

			require.Equal(t, tt.wantVersion, req.GetVersionInfo())
			require.Equal(t, strconv.Itoa(len(tt.responses)), req.GetResponseNonce())
			require.Equal(t, tt.wantRejection, req.GetErrorDetail() != nil)
			require.Equal(t, tt.wantBuilds, client.stats.builds)
			require.Equal(t, tt.wantSkipped, client.stats.skipped)
			require.Equal(t, tt.wantUnbuilt, client.stats.unbuilt)
			require.Equal(t, tt.wantApplied, client.applied.GetConfig().GetVmConfig().GetVmId())
		})
	}
}

func TestECDSClientStreams(t *testing.T) {
	newClient := func(buildErr error) *ecdsClient {
		return &ecdsClient{resourceName: modelResourceName, node: "node", build: func(*wasmhttp.Wasm) error { return buildErr }}
	}

	t.Run("only the first request of a stream carries the node", func(t *testing.T) {
		client := newClient(nil)
		client.streamEstablished()
		require.Equal(t, "node", client.request().GetNode().GetId())
		client.sent()
		require.Nil(t, client.request().GetNode())
	})

	t.Run("a stream reports the last accepted version and no nonce", func(t *testing.T) {
		client := newClient(nil)
		client.onResponse(ecdsResponse(t, modelResourceName, "v1", "1", "a"))
		client.sent()
		require.Equal(t, 1, client.stats.acks)

		client.streamEstablished()
		req := client.request()
		require.Equal(t, "v1", req.GetVersionInfo())
		require.Empty(t, req.GetResponseNonce())
		require.Nil(t, req.GetErrorDetail())
	})

	t.Run("a rejection that was not sent opens the next stream without a nonce", func(t *testing.T) {
		client := newClient(errCodeNotCached)
		client.onResponse(ecdsResponse(t, modelResourceName, "v1", "1", "a"))
		req := client.request()
		require.Equal(t, "1", req.GetResponseNonce())
		require.NotNil(t, req.GetErrorDetail())

		client.streamEstablished()
		req = client.request()
		require.Empty(t, req.GetVersionInfo())
		require.Empty(t, req.GetResponseNonce())
		require.Equal(t, errCodeNotCached.Error(), req.GetErrorDetail().GetMessage())
		client.sent()
		require.Equal(t, 1, client.stats.nacks)
	})

	t.Run("a rejection that was sent is not replayed", func(t *testing.T) {
		client := newClient(errCodeNotCached)
		client.onResponse(ecdsResponse(t, modelResourceName, "v1", "1", "a"))
		client.sent()
		require.Equal(t, 1, client.stats.nacks)

		client.streamEstablished()
		require.Nil(t, client.request().GetErrorDetail())
	})
}

// wasmFilter is a Wasm filter config.
func wasmFilter(nackOnCodeCacheMiss bool) *wasmhttp.Wasm {
	return &wasmhttp.Wasm{Config: &wasmv3.PluginConfig{
		Vm: &wasmv3.PluginConfig_VmConfig{VmConfig: &wasmv3.VmConfig{NackOnCodeCacheMiss: nackOnCodeCacheMiss}},
	}}
}

func TestWasmModuleFetchesUntilCached(t *testing.T) {
	needsModule, standalone := wasmFilter(true), wasmFilter(false)

	t.Run("a build fails until the fetch ends", func(t *testing.T) {
		module := &wasmModule{fetchTime: 20 * time.Millisecond}
		require.NoError(t, module.build(standalone))
		require.ErrorIs(t, module.build(needsModule), errCodeNotCached, "the first build starts the fetch")
		require.ErrorIs(t, module.build(needsModule), errCodeNotCached, "the fetch is still running")
		require.Eventually(t, func() bool { return module.build(needsModule) == nil }, 5*time.Second, 5*time.Millisecond)
		require.Equal(t, 1, module.fetches)
	})

	t.Run("a fetch that fails is started again by the next build", func(t *testing.T) {
		module := &wasmModule{fetchTime: time.Millisecond, failFetches: 1}
		settled := func() bool {
			module.mu.Lock()
			defer module.mu.Unlock()
			return !module.fetching
		}
		require.Error(t, module.build(needsModule))
		require.Eventually(t, settled, 5*time.Second, time.Millisecond)
		require.Error(t, module.build(needsModule), "the failed fetch left the module uncached")
		require.Eventually(t, settled, 5*time.Second, time.Millisecond)
		require.NoError(t, module.build(needsModule))
		require.Equal(t, 2, module.fetches)
	})
}

// A proxy that starts without its module rejects the configuration it is sent
// first, and a single retry gives it a built filter.
func TestProxyModelAppliesAConfigurationAfterARejection(t *testing.T) {
	fixture := newStreamFixture(t)
	// The retry waits for the module fetch to end.
	fixture.server.RetryDelay = 100 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	proxy := newProxyModel(fixture.resourceName, "cold-proxy", fixture.newECDSClient(t))
	proxy.module.fetchTime = 20 * time.Millisecond
	proxy.start(t)

	require.Eventually(t, func() bool { return proxy.state().applied != nil }, 5*time.Second, 5*time.Millisecond)
	state := proxy.state()
	require.Equal(t, 1, state.stats.nacks)
	require.Zero(t, state.stats.unbuilt)
}

// A proxy never accepts bytes it did not build. A rejection is parked, so the
// cache sends nothing for it, instead of the bytes the proxy just rejected.
func TestProxyModelNeverAcceptsBytesItDidNotBuild(t *testing.T) {
	fixture := newStreamFixture(t)
	// Keep the retry out of the way.
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	proxy := newProxyModel(fixture.resourceName, "rejecting-proxy", fixture.newECDSClient(t))
	proxy.module.failFetches = neverFetched
	proxy.start(t)

	require.Eventually(t, fixture.retryPending, 5*time.Second, 5*time.Millisecond)
	require.Never(t, func() bool { return proxy.state().stats.responses > 1 }, 300*time.Millisecond, 10*time.Millisecond)
	state := proxy.state()
	require.Zero(t, state.stats.unbuilt)
	require.Empty(t, state.version)
	require.Nil(t, state.applied)
}

// snapshotWithContent is the fixture's snapshot of version with configuration
// bytes of its own, as new rules give: the fixture's other snapshots differ in
// their version only, and a proxy accepts them without building the filter again.
func (f *streamFixture) snapshotWithContent(t *testing.T, version string) *Snapshot {
	t.Helper()
	snapshot := f.snapshot(t, version)
	var config wasmhttp.Wasm
	require.NoError(t, snapshot.Resource.TypedConfig.UnmarshalTo(&config))
	var err error
	config.GetConfig().GetVmConfig().Configuration, err = anypb.New(wrapperspb.String(version))
	require.NoError(t, err)
	snapshot.Resource.TypedConfig, err = anypb.New(&config)
	require.NoError(t, err)
	return snapshot
}

// New content reaches a proxy that rejected the old content at once, without
// waiting out the retry delay.
func TestProxyModelReceivesNewContentWhileRejecting(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = time.Hour
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v1")))
	proxy := newProxyModel(fixture.resourceName, "waiting-proxy", fixture.newECDSClient(t))
	proxy.module.fetchTime = time.Millisecond
	proxy.start(t)
	require.Eventually(t, fixture.retryPending, 5*time.Second, 5*time.Millisecond)
	// The new content is built once the fetch the rejection started has ended.
	require.Eventually(t, proxy.module.isCached, 5*time.Second, time.Millisecond)

	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v2")))
	require.Eventually(t, func() bool { return strings.HasPrefix(proxy.state().version, "sha256:v2") }, 5*time.Second, 5*time.Millisecond)
	state := proxy.state()
	require.NotNil(t, state.applied)
	require.Zero(t, state.stats.unbuilt)
}

// builtContent is the configuration a filter is built from, which the fixture's
// snapshotWithContent sets to the name of its version.
func builtContent(config *wasmhttp.Wasm) (string, error) {
	var value wrapperspb.StringValue
	if err := config.GetConfig().GetVmConfig().GetConfiguration().UnmarshalTo(&value); err != nil {
		return "", err
	}
	return value.GetValue(), nil
}

// New content that the proxy rejects too is retried like the content before it:
// the proxy ends up with a built filter without another content change. The
// proxy rejects the first build of the new content whatever its module cache
// holds, so that it is rejected and then built, not built at once.
func TestProxyModelConvergesWhenNewContentIsAlsoRejected(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 50 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v1")))
	proxy := newProxyModel(fixture.resourceName, "rejecting-proxy", fixture.newECDSClient(t))
	proxy.module.fetchTime = 5 * time.Millisecond
	var rejectedV2 atomic.Int32
	moduleBuild := proxy.client.build
	proxy.client.build = func(config *wasmhttp.Wasm) error {
		built, err := builtContent(config)
		if err != nil {
			return err
		}
		if built == "v2" && rejectedV2.CompareAndSwap(0, 1) {
			return errCodeNotCached
		}
		return moduleBuild(config)
	}
	proxy.start(t)
	require.Eventually(t, func() bool { return proxy.state().stats.buildFailures >= 1 }, 5*time.Second, time.Millisecond)

	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v2")))
	require.Eventually(t, func() bool {
		state := proxy.state()
		return state.applied != nil && strings.HasPrefix(state.version, "sha256:v2")
	}, 5*time.Second, 5*time.Millisecond)
	state := proxy.state()
	require.Equal(t, int32(1), rejectedV2.Load(), "the new content was rejected before it was built")
	built, err := builtContent(state.applied)
	require.NoError(t, err)
	require.Equal(t, "v2", built)
	require.GreaterOrEqual(t, state.stats.buildFailures, 2)
	require.Zero(t, state.stats.unbuilt)
}

// A proxy that has rejected a configuration for long, so that its back-off is at
// its longest, starts again from the first step when the Engine has other
// content: the content that replaces the rejected one is not delivered, rejected
// again, say because the module is still being published, and then left waiting
// the longest step. Rejections of the same content still escalate.
func TestProxyModelBackoffRestartsWhenTheContentChanges(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 50 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v1")))
	// The proxy was sent this configuration before, by a process that is gone,
	// and has rejected it long enough for its next rejection to wait the longest
	// step, 30 seconds.
	const longLadder = 40
	engineKey := testNamespace + "/" + streamEngine
	fixture.server.deliveries.sent(proxyCacheKey(engineKey, fixture.identity, "proxy"), engineKey, "sha256:v1", tag{epoch: otherProcessEpoch})
	fixture.seedRejections(t, "proxy", longLadder)
	proxy := newProxyModel(fixture.resourceName, "proxy", fixture.newECDSClient(t))
	proxy.module.failFetches = neverFetched
	proxy.start(t)
	require.Eventually(t, fixture.retryPending, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, longLadder+1, fixture.record("proxy").attempts)

	// New content ends the wait and is delivered; its rejection starts the ladder
	// again, and the later ones escalate it from there.
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v2")))
	deliveries := func() int {
		count := 0
		for _, version := range proxy.state().received {
			if strings.HasPrefix(version, "sha256:v2") {
				count++
			}
		}
		return count
	}
	require.Eventually(t, func() bool { return deliveries() >= 3 }, 5*time.Second, 10*time.Millisecond,
		"the rejection of the new content waited out a long step")
	attempts := fixture.record("proxy").attempts
	require.Greater(t, attempts, 1, "rejections of the same content escalate")
	require.Less(t, attempts, 10, "and start from the first step")
	require.Zero(t, proxy.state().stats.unbuilt)
}

// awaitRejection starts the proxy and waits until the fixture holds its
// rejection and the proxy's module is cached, so that its next build succeeds.
func (f *streamFixture) awaitRejection(t *testing.T, proxy *proxyModel) {
	t.Helper()
	proxy.start(t)
	require.Eventually(t, f.retryPending, 5*time.Second, 5*time.Millisecond)
	require.Eventually(t, proxy.module.isCached, 5*time.Second, time.Millisecond)
}

// requireAllOn waits until every proxy has the version of content that starts with prefix.
func requireAllOn(t *testing.T, proxies []*proxyModel, prefix string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, proxy := range proxies {
			if !strings.HasPrefix(proxy.state().version, prefix) {
				return false
			}
		}
		return true
	}, 10*time.Second, 10*time.Millisecond)
}

// requireConverged waits until the proxy has built a filter, and requires that
// it never accepted bytes it did not build on the way.
func requireConverged(t *testing.T, proxy *proxyModel) {
	t.Helper()
	require.Eventually(t, func() bool { return proxy.state().applied != nil }, 5*time.Second, 5*time.Millisecond)
	require.Zero(t, proxy.state().stats.unbuilt)
}

// A proxy whose stream ends while it waits out the retry delay, whichever side
// ends it, does not wait for the delay and ends up with a built filter.
func TestProxyModelConvergesWhenTheStreamEndsDuringTheBackoff(t *testing.T) {
	tests := []struct {
		name string
		end  func(*streamFixture, *proxyModel)
	}{
		{"the proxy ends the stream", func(_ *streamFixture, proxy *proxyModel) { proxy.dropStream() }},
		{"the server ends the stream", func(fixture *streamFixture, _ *proxyModel) { fixture.streams.CloseAll() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newStreamFixture(t)
			fixture.server.RetryDelay = time.Hour
			require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
			proxy := newProxyModel(fixture.resourceName, "proxy", fixture.newECDSClient(t))
			proxy.module.fetchTime = 5 * time.Millisecond
			fixture.awaitRejection(t, proxy)

			tt.end(fixture, proxy)
			requireConverged(t, proxy)
		})
	}
}

// A proxy that moves to another operator replica, or finds the operator
// restarted, is sent bytes of the new process if it rejected the old ones, and
// the bytes it built if it did not: its filter is not built again.
func TestProxyModelMovesToAnotherServer(t *testing.T) {
	tests := []struct {
		name      string
		rejecting bool // the proxy rejected the first server's configuration
		stopFirst bool // the first server is gone, as in a restart
	}{
		{"a rejecting proxy moves to another replica", true, false},
		{"a rejecting proxy outlives an operator restart", true, true},
		{"a healthy proxy moves to another replica", false, false},
		{"a healthy proxy outlives an operator restart", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, second := newStreamFixture(t), newStreamFixture(t)
			for _, fixture := range []*streamFixture{first, second} {
				fixture.server.RetryDelay = time.Hour
				require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
			}
			proxy := newProxyModel(first.resourceName, "proxy", first.newECDSClient(t))
			if tt.rejecting {
				proxy.module.fetchTime = 5 * time.Millisecond
				first.awaitRejection(t, proxy)
			} else {
				proxy.module.cached = true
				proxy.start(t)
				requireConverged(t, proxy)
			}

			if tt.stopFirst {
				first.cancel()
			}
			proxy.connectTo(second.newECDSClient(t))
			proxy.dropStream()
			if tt.rejecting {
				requireConverged(t, proxy)
				return
			}
			require.Eventually(t, func() bool { return proxy.state().stats.skipped == 1 }, 5*time.Second, 5*time.Millisecond)
			state := proxy.state()
			require.Equal(t, 1, state.stats.builds, "the filter was not built again")
			require.Zero(t, state.stats.unbuilt)
		})
	}
}

func TestProxyModelKeepsItsFilterAcrossAReconnect(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	proxy := newProxyModel(fixture.resourceName, "proxy", fixture.newECDSClient(t))
	proxy.module.cached = true
	proxy.start(t)
	requireConverged(t, proxy)

	proxy.dropStream()
	require.Eventually(t, func() bool { return proxy.state().stats.skipped == 1 }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, 1, proxy.state().stats.builds)
}

// A proxy that rejects every configuration is never sent bytes it rejected: no
// response is accepted unbuilt, each carries a tag of its own, and the Engine
// holds few tag snapshots however long the proxy goes on.
func TestProxyModelRejectingEveryConfigurationIsNeverSentTheSameBytes(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 10 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	proxy := newProxyModel(fixture.resourceName, "rejecting-proxy", fixture.newECDSClient(t))
	proxy.module.failFetches = neverFetched
	proxy.start(t)

	require.Eventually(t, func() bool { return proxy.state().stats.nacks >= 6 }, 10*time.Second, 5*time.Millisecond)
	state := proxy.state()
	require.Zero(t, state.stats.unbuilt)
	require.Zero(t, state.stats.acks)
	require.Nil(t, state.applied)
	distinct := make(map[string]struct{})
	for _, version := range state.received {
		distinct[version] = struct{}{}
	}
	require.Len(t, distinct, len(state.received), "a response repeated an earlier one")
	require.LessOrEqual(t, fixture.generation(testNamespace+"/"+streamEngine), uint64(state.stats.nacks)+1)
	require.LessOrEqual(t, fixture.heldTags(), 2)
}

// A rejection that never reached the server, because the stream ended after the
// proxy sent it or the proxy never sent it, still gets the proxy other bytes.
func TestProxyModelConvergesWhenARejectionIsLost(t *testing.T) {
	tests := []struct {
		name string
		fate sendFate
	}{
		{"the stream ended after the proxy sent the rejection", lose},
		{"the proxy replays a rejection it did not send", refuse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newStreamFixture(t)
			fixture.server.RetryDelay = 20 * time.Millisecond
			require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
			proxy := newProxyModel(fixture.resourceName, "proxy", fixture.newECDSClient(t))
			proxy.module.fetchTime = 5 * time.Millisecond
			// The proxy reconnects once its module fetch has ended.
			proxy.reconnect = 50 * time.Millisecond
			var lost atomic.Bool
			proxy.beforeSend = func(req *discoveryv3.DiscoveryRequest) sendFate {
				if req.GetErrorDetail() != nil && lost.CompareAndSwap(false, true) {
					return tt.fate
				}
				return deliver
			}
			proxy.start(t)

			requireConverged(t, proxy)
			require.Equal(t, uint64(1), fixture.generation(testNamespace+"/"+streamEngine))
		})
	}
}

// Many Gateway pods that start together are each rejected once, before their
// module is cached, and all converge. The Engine moves on at most once for each
// tag the pods rejected, and none of them accepts bytes it did not build. Pods
// that settle on different tags keep one snapshot per tag while they are
// connected, so how many remain depends on how the pods' rejections interleave.
func TestProxyModelFortyProxiesStartingTogether(t *testing.T) {
	const pods = 40
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 50 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	proxies := make([]*proxyModel, pods)
	for i := range proxies {
		proxies[i] = newProxyModel(fixture.resourceName, fmt.Sprintf("pod-%d", i), fixture.newECDSClient(t))
		proxies[i].module.fetchTime = 20 * time.Millisecond
	}
	for _, proxy := range proxies {
		proxy.start(t)
	}

	require.Eventually(t, func() bool {
		for _, proxy := range proxies {
			if proxy.state().applied == nil {
				return false
			}
		}
		return true
	}, 10*time.Second, 10*time.Millisecond)
	for _, proxy := range proxies {
		require.Zero(t, proxy.state().stats.unbuilt)
	}
	require.LessOrEqual(t, fixture.generation(testNamespace+"/"+streamEngine), uint64(pods))
	require.LessOrEqual(t, fixture.heldTags(), pods)
	require.Zero(t, testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalResponseBudget)))
}

// Pods that start one at a time are each rejected once, before their module is
// cached, so each ends on a tag of its own. A pod that connects again reports
// the tag it built and is sent the same bytes, whatever the number of tags: it
// does not build its filter again when its stream ends, nor when the operator
// restarts and another process serves it. New content reaches every pod, under
// the tag it holds.
func TestProxyModelPodsOnDistinctTagsKeepTheirFilters(t *testing.T) {
	const pods = 16
	first, second := newStreamFixture(t), newStreamFixture(t)
	for _, fixture := range []*streamFixture{first, second} {
		fixture.server.RetryDelay = 20 * time.Millisecond
		require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v1")))
	}
	proxies := make([]*proxyModel, pods)
	tags := make(map[string]struct{})
	for i := range proxies {
		proxies[i] = newProxyModel(first.resourceName, fmt.Sprintf("pod-%d", i), first.newECDSClient(t))
		proxies[i].module.fetchTime = 5 * time.Millisecond
		proxies[i].start(t)
		requireConverged(t, proxies[i])
		tags[proxies[i].state().version] = struct{}{}
	}
	require.Len(t, tags, pods, "each pod settled on a tag of its own")
	require.Equal(t, pods, first.heldTags())

	reconnect := func(name string, end func(*proxyModel)) {
		before := make([]proxyStats, pods)
		for i, proxy := range proxies {
			before[i] = proxy.state().stats
		}
		for _, proxy := range proxies {
			end(proxy)
		}
		require.Eventually(t, func() bool {
			for i, proxy := range proxies {
				if proxy.state().stats.responses == before[i].responses {
					return false
				}
			}
			return true
		}, 10*time.Second, 5*time.Millisecond, name)
		for i, proxy := range proxies {
			after := proxy.state().stats
			require.Equal(t, before[i].builds, after.builds, "%s: pod %d built its filter again", name, i)
			require.Equal(t, before[i].skipped+1, after.skipped, "%s: pod %d was sent the bytes it built", name, i)
			require.Zero(t, after.unbuilt)
		}
	}
	reconnect("the streams end at their lifetime", func(proxy *proxyModel) { proxy.dropStream() })
	require.Equal(t, pods, first.heldTags(), "each pod kept its tag")

	first.cancel()
	reconnect("the operator restarts", func(proxy *proxyModel) {
		proxy.connectTo(second.newECDSClient(t))
		proxy.dropStream()
	})
	require.Equal(t, pods, second.heldTags(), "the new process serves every tag the pods built")

	require.NoError(t, second.store.Replace(second.snapshotWithContent(t, "v2")))
	requireAllOn(t, proxies, "sha256:v2")
	for _, proxy := range proxies {
		require.Zero(t, proxy.state().stats.unbuilt)
	}
	require.Equal(t, pods, second.heldTags())
}

// A proxy that moves to another replica, which replays the bytes it built, is
// sent new content under that replica's own tag. When it rejects the new content
// there and returns to the replica it came from, which has the new content too,
// that replica does not send it the bytes it rejected: they would have carried the
// tag of the first replica, and Envoy accepts bytes it already received without
// building them.
func TestProxyModelReturningToAReplicaDoesNotMeetTheBytesItRejectedOnAnother(t *testing.T) {
	first, second := newStreamFixture(t), newStreamFixture(t)
	for _, fixture := range []*streamFixture{first, second} {
		fixture.server.RetryDelay = time.Hour
		require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v1")))
	}
	proxy := newProxyModel(first.resourceName, "proxy", first.newECDSClient(t))
	proxy.module.cached = true
	var rejectedV2 atomic.Bool
	proxy.client.build = func(config *wasmhttp.Wasm) error {
		built, err := builtContent(config)
		if err != nil {
			return err
		}
		if built == "v2" && rejectedV2.CompareAndSwap(false, true) {
			return errCodeNotCached
		}
		return nil
	}
	proxy.start(t)
	requireConverged(t, proxy)
	onFirst := proxy.state().version

	// The second replica replays the bytes the first sent, which are not built again.
	proxy.connectTo(second.newECDSClient(t))
	proxy.dropStream()
	require.Eventually(t, func() bool { return proxy.state().stats.responses >= 2 }, 5*time.Second, time.Millisecond)
	require.Equal(t, onFirst, proxy.state().version)
	require.Equal(t, 1, proxy.state().stats.builds)

	// Both replicas publish the new content. The proxy is sent it by the second,
	// under the second's own tag, and rejects it.
	require.NoError(t, first.store.Replace(first.snapshotWithContent(t, "v2")))
	require.NoError(t, second.store.Replace(second.snapshotWithContent(t, "v2")))
	require.Eventually(t, rejectedV2.Load, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return proxy.state().stats.buildFailures == 1 }, 5*time.Second, time.Millisecond)
	rejected := proxy.state().received[len(proxy.state().received)-1]
	_, sent, tagged := parseWireVersion(rejected)
	require.True(t, tagged)
	require.Equal(t, second.store.epoch, sent.epoch, "the second replica sent the new content under its own tag")

	// Back at the first replica, the proxy builds the new content.
	proxy.connectTo(first.newECDSClient(t))
	proxy.dropStream()
	require.Eventually(t, func() bool { return strings.HasPrefix(proxy.state().version, "sha256:v2") }, 5*time.Second, time.Millisecond)
	state := proxy.state()
	require.Zero(t, state.stats.unbuilt, "the first replica sent bytes the proxy rejected on the second")
	require.NotEqual(t, rejected, state.version)
	built, err := builtContent(state.applied)
	require.NoError(t, err)
	require.Equal(t, "v2", built)
}

// Gateways that move between two replicas, one after another, while the content
// is published on both build their filter once for each content, however the moves
// and the publication interleave: a proxy that reports the tag of the replica it
// came from, for the content it built there, is replayed those bytes by the other
// replica even while that replica holds the tag for proxies of the older content.
func TestProxyModelGatewaysMovingBetweenReplicasBuildOncePerContent(t *testing.T) {
	for _, mode := range []string{"alternating", "random"} {
		t.Run(mode, func(t *testing.T) {
			const pods, cycles = 8, 6
			replicas := []*streamFixture{newStreamFixture(t), newStreamFixture(t)}
			for _, replica := range replicas {
				require.NoError(t, replica.store.Replace(replica.snapshotWithContent(t, "v1")))
			}
			choose := rand.New(rand.NewPCG(1, 2))
			on := make([]int, pods)
			proxies := make([]*proxyModel, pods)
			for i := range proxies {
				on[i] = i % len(replicas)
				proxies[i] = newProxyModel(replicas[on[i]].resourceName, fmt.Sprintf("pod-%d", i), replicas[on[i]].newECDSClient(t))
				proxies[i].module.cached = true
				proxies[i].reconnect = time.Millisecond
				proxies[i].start(t)
				requireConverged(t, proxies[i])
			}
			move := func(i int) {
				if mode == "alternating" {
					on[i] = 1 - on[i]
				} else {
					on[i] = choose.IntN(len(replicas))
				}
				before := proxies[i].state().stats.responses
				proxies[i].connectTo(replicas[on[i]].newECDSClient(t))
				proxies[i].dropStream()
				require.Eventually(t, func() bool { return proxies[i].state().stats.responses > before }, 5*time.Second, time.Millisecond)
			}
			for cycle := range cycles {
				if cycle == cycles/2 {
					for _, replica := range replicas {
						require.NoError(t, replica.store.Replace(replica.snapshotWithContent(t, "v2")))
					}
					requireAllOn(t, proxies, "sha256:v2")
				}
				for i := range proxies {
					move(i)
				}
			}
			for i, proxy := range proxies {
				state := proxy.state()
				require.Zero(t, state.stats.unbuilt)
				require.Equal(t, 2, state.stats.builds, "pod %d built its filter once for each content", i)
			}
		})
	}
}

// A proxy that reports a version of an earlier release, one that is a bare
// content hash or a retry of one, is sent a tagged configuration on its first
// reconnect: a Gateway stranded on bytes it never built heals.
func TestProxyModelReportingAnEarlierReleaseVersionIsSentATag(t *testing.T) {
	for _, reported := range []string{"sha256:v1", "sha256:v1.retry2"} {
		t.Run(reported, func(t *testing.T) {
			fixture := newStreamFixture(t)
			require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
			proxy := newProxyModel(fixture.resourceName, "proxy", fixture.newECDSClient(t))
			proxy.module.cached = true
			proxy.client.version = reported
			proxy.start(t)

			requireConverged(t, proxy)
			state := proxy.state()
			_, sent, tagged := parseWireVersion(state.version)
			require.True(t, tagged, "version %q", state.version)
			require.Equal(t, tag{epoch: fixture.store.epoch}, sent)
			require.Equal(t, 1, state.stats.builds)
			require.Equal(t, fixture.vmID(0), state.applied.GetConfig().GetVmConfig().GetVmId())
		})
	}
}

// A proxy that accepted one content and rejects the next, and whose rejection
// never reached the server, reports the old content's version when it connects
// again. The Engine has no such content any more, so that report is no word that
// the proxy built the new bytes, which it must not be sent again.
func TestProxyModelLosingTheRejectionOfNewContent(t *testing.T) {
	fixture := newStreamFixture(t)
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v1")))
	proxy := newProxyModel(fixture.resourceName, "proxy", fixture.newECDSClient(t))
	proxy.module.cached = true
	// The proxy builds the first content, and rejects the second until the
	// rejection is lost.
	var lost atomic.Bool
	proxy.client.build = func(config *wasmhttp.Wasm) error {
		built, err := builtContent(config)
		if err != nil {
			return err
		}
		if built == "v2" && !lost.Load() {
			return errCodeNotCached
		}
		return nil
	}
	proxy.beforeSend = func(req *discoveryv3.DiscoveryRequest) sendFate {
		if req.GetErrorDetail() != nil && lost.CompareAndSwap(false, true) {
			return lose
		}
		return deliver
	}
	proxy.start(t)
	requireConverged(t, proxy)

	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v2")))
	require.Eventually(t, func() bool { return strings.HasPrefix(proxy.state().version, "sha256:v2") }, 5*time.Second, 5*time.Millisecond)
	require.Zero(t, proxy.state().stats.unbuilt)
}
