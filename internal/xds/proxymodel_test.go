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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	wasmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
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

	stats proxyStats
}

// streamEstablished starts a stream: the nonce is cleared, and the version and
// an unsent rejection are kept.
func (c *ecdsClient) streamEstablished() {
	c.nonce, c.opening = "", true
}

// onResponse processes one response the way the subscription does.
func (c *ecdsClient) onResponse(resp *discoveryv3.DiscoveryResponse) {
	c.stats.responses++
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

// proxyModel is a proxy that keeps a stream to a test server open, and opens a
// new one when it ends. Give each proxy its own connection, as each Gateway pod
// has one: a server limits the streams of a connection.
type proxyModel struct {
	rpc       extensionv3.ExtensionConfigDiscoveryServiceClient
	reconnect time.Duration
	module    *wasmModule

	mu     sync.Mutex
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
	stream, err := p.rpc.StreamExtensionConfigs(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+validToken))
	if err != nil {
		return
	}
	p.mu.Lock()
	p.client.streamEstablished()
	req := p.client.request()
	p.mu.Unlock()
	for {
		// A request the stream refuses is not sent, so a rejection stays unsent.
		if stream.Send(req) != nil {
			return
		}
		p.mu.Lock()
		p.client.sent()
		p.mu.Unlock()
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
	require.Eventually(t, func() bool {
		proxy.module.mu.Lock()
		defer proxy.module.mu.Unlock()
		return proxy.module.cached
	}, 5*time.Second, time.Millisecond)

	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v2")))
	require.Eventually(t, func() bool { return strings.HasPrefix(proxy.state().version, "sha256:v2") }, 5*time.Second, 5*time.Millisecond)
	state := proxy.state()
	require.NotNil(t, state.applied)
	require.Zero(t, state.stats.unbuilt)
}

// New content that the proxy rejects too is retried like the content before it:
// the proxy ends up with a built filter without another content change.
func TestProxyModelConvergesWhenNewContentIsAlsoRejected(t *testing.T) {
	fixture := newStreamFixture(t)
	fixture.server.RetryDelay = 50 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v1")))
	proxy := newProxyModel(fixture.resourceName, "rejecting-proxy", fixture.newECDSClient(t))
	// The first fetch ends without the module, so the next configuration is
	// rejected as well.
	proxy.module.fetchTime = 5 * time.Millisecond
	proxy.module.failFetches = 1
	proxy.start(t)
	require.Eventually(t, func() bool { return proxy.state().stats.buildFailures == 1 }, 5*time.Second, time.Millisecond)

	require.NoError(t, fixture.store.Replace(fixture.snapshotWithContent(t, "v2")))
	require.Eventually(t, func() bool {
		state := proxy.state()
		return state.applied != nil && strings.HasPrefix(state.version, "sha256:v2")
	}, 5*time.Second, 5*time.Millisecond)
	state := proxy.state()
	require.GreaterOrEqual(t, state.stats.buildFailures, 2)
	require.Zero(t, state.stats.unbuilt)
}
