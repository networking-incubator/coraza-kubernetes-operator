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
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	authv1 "k8s.io/api/authentication/v1"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

const unaryECDSMethod = extensionv3.ExtensionConfigDiscoveryService_FetchExtensionConfigs_FullMethodName

// rawResult is how the server ended one HTTP/2 stream.
type rawResult struct {
	grpcStatus string
	reset      bool
	at         time.Time
}

// rawH2 is a minimal HTTP/2 client that writes frames directly, so a test can
// send one-byte DATA frames, withhold flow-control credit or stop mid-message.
type rawH2 struct {
	t       *testing.T
	writeMu sync.Mutex
	framer  *http2.Framer
	encoder *hpack.Encoder
	block   bytes.Buffer
	nextID  uint32
	// received counts response DATA bytes on every stream.
	received atomic.Int64

	mu     sync.Mutex
	ended  map[uint32]chan rawResult
	status map[uint32]string
	pongs  chan struct{} // PING acknowledgements
}

// dialRawH2 opens an h2 connection to the listener. initialWindow is the
// stream receive window the client advertises; zero means the server can
// never send response DATA.
func dialRawH2(t *testing.T, fixture *listenerFixture, initialWindow uint32) *rawH2 {
	t.Helper()
	conn, err := tls.Dial("tcp", fixture.listener.Addr().String(), &tls.Config{
		RootCAs: fixture.pool, ServerName: "localhost", NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS13,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.Write([]byte(http2.ClientPreface))
	require.NoError(t, err)

	c := &rawH2{t: t, framer: http2.NewFramer(conn, conn), nextID: 1, ended: map[uint32]chan rawResult{}, status: map[uint32]string{},
		pongs: make(chan struct{}, 1)}
	c.encoder = hpack.NewEncoder(&c.block)
	c.framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	require.NoError(t, c.framer.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: initialWindow}))

	settled := make(chan struct{})
	go c.readLoop(settled)
	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("no SETTINGS from the server")
	}
	return c
}

func (c *rawH2) readLoop(settled chan struct{}) {
	var once sync.Once
	for {
		frame, err := c.framer.ReadFrame()
		if err != nil {
			return
		}
		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				c.writeMu.Lock()
				_ = c.framer.WriteSettingsAck()
				c.writeMu.Unlock()
				once.Do(func() { close(settled) })
			}
		case *http2.PingFrame:
			if f.IsAck() {
				select {
				case c.pongs <- struct{}{}:
				default:
				}
			} else {
				c.writeMu.Lock()
				_ = c.framer.WritePing(true, f.Data)
				c.writeMu.Unlock()
			}
		case *http2.MetaHeadersFrame:
			for _, field := range f.Fields {
				if field.Name == "grpc-status" {
					c.mu.Lock()
					c.status[f.StreamID] = field.Value
					c.mu.Unlock()
				}
			}
			if f.StreamEnded() {
				c.end(f.StreamID, rawResult{grpcStatus: c.statusOf(f.StreamID)})
			}
		case *http2.DataFrame:
			c.received.Add(int64(len(f.Data())))
			if f.StreamEnded() {
				c.end(f.StreamID, rawResult{grpcStatus: c.statusOf(f.StreamID)})
			}
		case *http2.RSTStreamFrame:
			c.end(f.StreamID, rawResult{grpcStatus: c.statusOf(f.StreamID), reset: true})
		}
	}
}

func (c *rawH2) statusOf(id uint32) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status[id]
}

// end records the first way a stream ended; later frames for it are ignored.
func (c *rawH2) end(id uint32, result rawResult) {
	result.at = time.Now()
	select {
	case c.channel(id) <- result:
	default:
	}
}

func (c *rawH2) channel(id uint32) chan rawResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.ended[id]
	if !ok {
		ch = make(chan rawResult, 1)
		c.ended[id] = ch
	}
	return ch
}

// open starts a request and returns its stream ID. headers are extra
// name/value pairs such as authorization.
func (c *rawH2) open(method, path, contentType string, endStream bool, headers ...string) uint32 {
	c.t.Helper()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.block.Reset()
	fields := []string{":method", method, ":scheme", "https", ":authority", "localhost", ":path", path}
	if contentType != "" {
		fields = append(fields, "content-type", contentType, "te", "trailers")
	}
	fields = append(fields, headers...)
	for i := 0; i < len(fields); i += 2 {
		require.NoError(c.t, c.encoder.WriteField(hpack.HeaderField{Name: fields[i], Value: fields[i+1]}))
	}
	id := c.nextID
	c.nextID += 2
	c.channel(id)
	require.NoError(c.t, c.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID: id, BlockFragment: c.block.Bytes(), EndHeaders: true, EndStream: endStream,
	}))
	return id
}

// openGRPC starts a gRPC request to path with an optional bearer token.
func (c *rawH2) openGRPC(path, token string) uint32 {
	var headers []string
	if token != "" {
		headers = []string{"authorization", "Bearer " + token}
	}
	return c.open("POST", path, grpcContentType, false, headers...)
}

// data sends p on stream id in DATA frames of at most frameSize bytes.
func (c *rawH2) data(id uint32, p []byte, frameSize int) {
	c.t.Helper()
	for len(p) > 0 {
		n := min(frameSize, len(p))
		c.writeMu.Lock()
		err := c.framer.WriteData(id, false, p[:n])
		c.writeMu.Unlock()
		require.NoError(c.t, err)
		p = p[n:]
	}
}

// ping returns once the server acknowledges a PING, so it has handled every
// frame sent before it.
func (c *rawH2) ping() {
	c.t.Helper()
	c.writeMu.Lock()
	err := c.framer.WritePing(false, [8]byte{})
	c.writeMu.Unlock()
	require.NoError(c.t, err)
	select {
	case <-c.pongs:
	case <-time.After(5 * time.Second):
		c.t.Fatal("the server did not acknowledge a PING within 5s")
	}
}

// reset cancels stream id.
func (c *rawH2) reset(id uint32) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.framer.WriteRSTStream(id, http2.ErrCodeCancel)
}

func (c *rawH2) wait(id uint32, timeout time.Duration) rawResult {
	c.t.Helper()
	select {
	case result := <-c.channel(id):
		return result
	case <-time.After(timeout):
		c.t.Fatalf("stream %d did not end within %v", id, timeout)
		return rawResult{}
	}
}

// grpcFrame prefixes message with the gRPC length-prefixed message header.
func grpcFrame(t *testing.T, message proto.Message) []byte {
	t.Helper()
	payload, err := proto.Marshal(message)
	require.NoError(t, err)
	return append(grpcHeader(len(payload)), payload...)
}

func grpcHeader(length int) []byte {
	header := make([]byte, grpcMessageHeaderBytes)
	binary.BigEndian.PutUint32(header[1:], uint32(length))
	return header
}

// heapBytes returns the live heap after a full collection.
func heapBytes() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

func heapGrowth(before, after uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}

func (f *listenerFixture) registeredStreams() int {
	f.streams.mu.Lock()
	defer f.streams.mu.Unlock()
	return len(f.streams.streams)
}

// requireNoStreams waits until no request holds an open-stream or registry
// slot.
func (f *listenerFixture) requireNoStreams(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		return f.server.openStreams.Load() == 0 && f.registeredStreams() == 0
	}, 5*time.Second, 10*time.Millisecond, "open streams = %d, registered = %d", f.server.openStreams.Load(), f.registeredStreams())
}

// Refused requests are answered from their headers: neither an unauthenticated
// stream nor a unary request has its body read, so partial uploads hold no
// memory once answered, and little while authentication is pending.
func TestListenerRefusesRequestsBeforeReadingBodies(t *testing.T) {
	fixture := newListenerFixture(t)
	fixture.setTokenDelay(50 * time.Millisecond)
	client := dialRawH2(t, fixture, 65535)

	// Each request announces a 1 MiB message and sends 16 KiB of it; 32 of
	// them stay inside the server's 1 MiB connection window.
	const requests = 32
	partial := append(grpcHeader(1<<20), make([]byte, 16<<10)...)
	before := heapBytes()
	started := time.Now()
	streamIDs := map[uint32]string{}
	for i := range requests {
		var id uint32
		if i%2 == 0 {
			id = client.openGRPC(ecdsStreamMethod, fmt.Sprintf("invalid-%d", i))
			streamIDs[id] = strconv.Itoa(int(codes.Unauthenticated))
		} else {
			id = client.openGRPC(unaryECDSMethod, validToken)
			streamIDs[id] = strconv.Itoa(int(codes.Unimplemented))
		}
		client.data(id, partial, h2MaxReadFrameSize)
	}
	pending := heapBytes()
	for id, want := range streamIDs {
		result := client.wait(id, 10*time.Second)
		require.Equal(t, want, result.grpcStatus, "stream %d", id)
	}
	elapsed := time.Since(started)
	after := heapBytes()
	t.Logf("%d partial uploads: heap growth %d KiB while pending, %d KiB after; all answered in %v",
		requests, heapGrowth(before, pending)>>10, heapGrowth(before, after)>>10, elapsed)

	require.Less(t, heapGrowth(before, pending), uint64(4<<20))
	require.Less(t, heapGrowth(before, after), uint64(2<<20))
	fixture.requireNoStreams(t)
	require.Equal(t, float64(requests/2), testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalMethod)))
}

// maxSizeRequest returns a valid subscription of exactly size bytes whose bulk
// is node metadata, the way Envoy's first request is.
func maxSizeRequest(t *testing.T, resourceName string, size int) *discoveryv3.DiscoveryRequest {
	t.Helper()
	padding := size
	for range 4 {
		nodeMetadata, err := structpb.NewStruct(map[string]any{"labels": strings.Repeat("x", max(padding, 0))})
		require.NoError(t, err)
		request := &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "test-proxy", Metadata: nodeMetadata}, ResourceNames: []string{resourceName}}
		got := proto.Size(request)
		if got == size {
			return request
		}
		padding += size - got
	}
	t.Fatalf("could not size a request to %d bytes", size)
	return nil
}

// An authenticated peer that sends a maximum-size request in one-byte DATA
// frames holds no more than about the message itself, and the request is
// still served once it completes.
func TestListenerBoundsOneByteFrameRequest(t *testing.T) {
	fixture := newListenerFixture(t)
	// Sending one byte per frame can outlast the 10s bounds on the first
	// request on a loaded host.
	fixture.listener.requestTimeout = time.Minute
	fixture.server.authDeadline = time.Minute
	require.NoError(t, fixture.store.Replace((&streamFixture{listenerFixture: fixture}).snapshot(t, "v1")))
	client := dialRawH2(t, fixture, 65535)

	limit := fixture.listener.limits.MaxRequestBytes
	message := grpcFrame(t, maxSizeRequest(t, fixture.resourceName, limit))
	before := heapBytes()
	started := time.Now()
	id := client.openGRPC(ecdsStreamMethod, validToken)
	client.data(id, message[:len(message)-1], 1)
	// Let the server consume what was sent before measuring.
	time.Sleep(200 * time.Millisecond)
	held := heapGrowth(before, heapBytes())
	t.Logf("%d-byte request in one-byte frames: heap growth %d KiB before its last byte (sent in %v)",
		limit, held>>10, time.Since(started))
	// Without the bound gRPC keeps a 16 KiB buffer per byte: about 4 GiB here.
	require.Less(t, held, uint64(64<<20), "heap held by an incomplete one-byte-frame request")

	client.data(id, message[len(message)-1:], 1)
	require.Eventually(t, func() bool { return fixture.registeredStreams() == 1 }, 5*time.Second, 10*time.Millisecond,
		"the completed request must be served")
}

func TestListenerRefusesOversizedRequest(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxRequestBytes = 1 << 10
	fixture := newListenerFixtureWithLimits(t, limits)
	client := dialRawH2(t, fixture, 65535)

	id := client.openGRPC(ecdsStreamMethod, validToken)
	client.data(id, grpcHeader(limits.MaxRequestBytes+1), h2MaxReadFrameSize)
	client.wait(id, 5*time.Second)
	require.Equal(t, float64(1), testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalRequestSize)))
	fixture.requireNoStreams(t)
}

func TestListenerCutsSlowRequest(t *testing.T) {
	fixture := newListenerFixture(t)
	fixture.listener.requestTimeout = 200 * time.Millisecond
	client := dialRawH2(t, fixture, 65535)

	id := client.openGRPC(ecdsStreamMethod, validToken)
	sent := time.Now()
	client.data(id, grpcHeader(64)[:3], 1)
	result := client.wait(id, 5*time.Second)
	t.Logf("a request stalled after three bytes ended after %v", result.at.Sub(sent))
	require.GreaterOrEqual(t, result.at.Sub(sent), 200*time.Millisecond)
	require.Equal(t, float64(1), testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalSlowRequest)))
	fixture.requireNoStreams(t)
}

// A peer that withholds flow-control credit is reset once a write blocks for
// the write timeout, and its slots are released, while an idle stream that
// reads normally outlives both the write and the request bounds and receives
// the next version.
func TestListenerResetsPeerThatStopsReading(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	limits.WriteTimeout = 300 * time.Millisecond
	fixture := newStreamFixtureWithLimits(t, limits)
	fixture.listener.requestTimeout = 300 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))

	healthy := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, healthy)
	send(t, healthy, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: first.GetVersionInfo(), ResponseNonce: first.GetNonce(),
	})

	stalled := dialRawH2(t, fixture.listenerFixture, 0)
	id := stalled.openGRPC(ecdsStreamMethod, validToken)
	sent := time.Now()
	stalled.data(id, grpcFrame(t, &discoveryv3.DiscoveryRequest{Node: &corev3.Node{Id: "stalled"}, ResourceNames: []string{fixture.resourceName}}), h2MaxReadFrameSize)
	result := stalled.wait(id, 5*time.Second)
	t.Logf("a peer with a zero receive window was reset after %v", result.at.Sub(sent))
	require.True(t, result.reset, "the stalled stream must be reset")
	require.Less(t, result.at.Sub(sent), 3*time.Second)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalWriteTimeout)) >= 1
	}, 5*time.Second, 10*time.Millisecond, "the reset must be counted as a write timeout")
	require.Eventually(t, func() bool {
		return fixture.server.openStreams.Load() == 1 && fixture.registeredStreams() == 1
	}, 5*time.Second, 10*time.Millisecond, "only the healthy stream may hold slots")

	time.Sleep(3 * limits.WriteTimeout)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	require.Equal(t, "sha256:v2", recv(t, healthy).GetVersionInfo())
}

func TestListenerResetsZeroWindowAssetRequest(t *testing.T) {
	limits := DefaultLimits()
	limits.WriteTimeout = 300 * time.Millisecond
	fixture := newListenerFixtureWithLimits(t, limits)
	client := dialRawH2(t, fixture, 0)

	sent := time.Now()
	id := client.open("GET", assetPathPrefix+fixture.manifest.SHA256+assetPathSuffix, "", true)
	result := client.wait(id, 5*time.Second)
	t.Logf("a zero-window asset request was reset after %v", result.at.Sub(sent))
	require.True(t, result.reset)
	require.Less(t, result.at.Sub(sent), 3*time.Second)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalWriteTimeout)) == 1
	}, 5*time.Second, 10*time.Millisecond, "the reset must be counted as a write timeout")
}

// dialStalledH2 opens an h2 connection with a tiny socket receive buffer and
// the largest flow-control windows, completes the SETTINGS exchange, and then
// never reads from the socket again: only TCP stops the server's writes.
func dialStalledH2(t *testing.T, fixture *listenerFixture) *rawH2 {
	t.Helper()
	dialer := &net.Dialer{Timeout: 2 * time.Second, Control: func(_, _ string, raw syscall.RawConn) error {
		var err error
		if controlErr := raw.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		}); controlErr != nil {
			return controlErr
		}
		return err
	}}
	tcp, err := dialer.Dial("tcp", fixture.listener.Addr().String())
	require.NoError(t, err)
	conn := tls.Client(tcp, &tls.Config{RootCAs: fixture.pool, ServerName: "localhost", NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS13})
	require.NoError(t, conn.Handshake())
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.Write([]byte(http2.ClientPreface))
	require.NoError(t, err)

	c := &rawH2{t: t, framer: http2.NewFramer(conn, conn), nextID: 1, ended: map[uint32]chan rawResult{}, status: map[uint32]string{}}
	c.encoder = hpack.NewEncoder(&c.block)
	c.framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	require.NoError(t, c.framer.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1<<31 - 1}))
	require.NoError(t, c.framer.WriteWindowUpdate(0, 1<<31-1-65535))
	for {
		frame, err := c.framer.ReadFrame()
		require.NoError(t, err)
		if settings, ok := frame.(*http2.SettingsFrame); ok && !settings.IsAck() {
			require.NoError(t, c.framer.WriteSettingsAck())
			return c
		}
	}
}

// serveLargeAsset replaces the bundled module with size bytes and returns
// its path.
func serveLargeAsset(fixture *listenerFixture, size int) string {
	data := bytes.Repeat([]byte{0xAB}, size)
	sum := sha256.Sum256(data)
	manifest := &assets.Manifest{SHA256: hex.EncodeToString(sum[:])}
	fixture.listener.handler = NewAssetHandler(manifest, data, fixture.metrics, fixture.listener.limits.WriteTimeout)
	return assetPathPrefix + manifest.SHA256 + assetPathSuffix
}

// A peer that stops reading at the TCP level cannot keep its streams: once a
// write makes no progress for the write timeout the connection is closed,
// its slots return and another Gateway is admitted.
func TestListenerClosesConnectionThatStopsReading(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	limits.WriteTimeout = 300 * time.Millisecond
	limits.MaxStreams = 4
	limits.MaxStreamsPerIdentity = 4
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	asset := serveLargeAsset(fixture.listenerFixture, 32<<20)

	stalled := dialStalledH2(t, fixture.listenerFixture)
	subscription := grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}})
	for range limits.MaxStreams {
		stalled.data(stalled.openGRPC(ecdsStreamMethod, validToken), subscription, h2MaxReadFrameSize)
	}
	require.Eventually(t, func() bool { return fixture.registeredStreams() == limits.MaxStreams }, 5*time.Second, 10*time.Millisecond)

	// The module download fills the socket buffers of a peer that never reads.
	// The connection closes once a write makes no progress for the write
	// timeout; closing TLS then waits up to 5s to send its close alert.
	stalled.open("GET", asset, "", true)
	started := time.Now()
	require.Eventually(t, func() bool {
		return fixture.server.openStreams.Load() == 0 && fixture.registeredStreams() == 0
	}, limits.WriteTimeout+10*time.Second, 10*time.Millisecond, "the stalled connection kept its stream slots")
	t.Logf("a TCP-stalled connection released its %d stream slots after %v", limits.MaxStreams, time.Since(started))

	stream := fixture.open(t, validToken, fixture.resourceName)
	require.Equal(t, "sha256:v1", recv(t, stream).GetVersionInfo())
}

// largeSnapshot is fixture's snapshot carrying size bytes of plugin
// configuration.
func (f *streamFixture) largeSnapshot(t *testing.T, version string, size int) *Snapshot {
	t.Helper()
	snapshot := f.snapshot(t, version)
	var wasm wasmhttp.Wasm
	require.NoError(t, snapshot.Resource.TypedConfig.UnmarshalTo(&wasm))
	configuration, err := anypb.New(wrapperspb.String(strings.Repeat("r", size)))
	require.NoError(t, err)
	wasm.Config.Configuration = configuration
	snapshot.Resource.TypedConfig, err = anypb.New(&wasm)
	require.NoError(t, err)
	return snapshot
}

// throttledConn reads at most bytesPerSecond.
type throttledConn struct {
	net.Conn
	bytesPerSecond int
}

func (c *throttledConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p[:min(len(p), 32<<10)])
	time.Sleep(time.Duration(n) * time.Second / time.Duration(c.bytesPerSecond))
	return n, err
}

// The write timeout bounds progress, not response size: a Gateway that reads
// a 10 MiB configuration at 2 MiB/s receives it under a 2s write timeout.
func TestListenerDeliversLargeResponseToSlowReader(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	limits.WriteTimeout = 2 * time.Second
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.largeSnapshot(t, "v1", 10<<20)))

	conn, err := grpc.NewClient(fixture.listener.Addr().String(),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: fixture.pool, ServerName: "localhost"})),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
		grpc.WithContextDialer(func(ctx context.Context, address string) (net.Conn, error) {
			tcp, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
			if err != nil {
				return nil, err
			}
			return &throttledConn{Conn: tcp, bytesPerSecond: 2 << 20}, nil
		}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	stream, err := extensionv3.NewExtensionConfigDiscoveryServiceClient(conn).StreamExtensionConfigs(
		metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken))
	require.NoError(t, err)
	started := time.Now()
	send(t, stream, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}})
	response := recv(t, stream)
	t.Logf("a %d MiB configuration reached a 2 MiB/s reader in %v", proto.Size(response)>>20, time.Since(started))
	require.Equal(t, "sha256:v1", response.GetVersionInfo())
	require.Zero(t, testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalWriteTimeout)))
}

// Garbage-token streams reset while their TokenReview is in flight are still
// charged, so a peer cannot review tokens past its failure budget by
// resetting.
func TestListenerChargesStreamsResetDuringReview(t *testing.T) {
	limits := DefaultLimits()
	limits.AuthFailureBurst = 50
	limits.AuthFailureRate = 1
	fixture := newListenerFixtureWithLimits(t, limits)
	release := make(chan struct{})
	releaseReview := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseReview)
	var reviews atomic.Int64
	var reviewing sync.Map // tokens whose review has started
	var firstAnswered atomic.Bool
	fixture.server.Auth.tokenReviews = reviewFunc(func(ctx context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error) {
		reviews.Add(1)
		reviewing.Store(review.Spec.Token, true)
		if review.Spec.Token == "rejected-first" {
			select {
			case <-release:
			case <-ctx.Done():
			}
			firstAnswered.Store(true)
		} else {
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
			}
		}
		// Like a real review call, one whose context ended fails.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return rejectedReview(), nil
	})
	client := dialRawH2(t, fixture, 65535)

	// The first stream is reset during its review, which ends only after the
	// server has handled the reset.
	started := time.Now()
	first := client.openGRPC(ecdsStreamMethod, "rejected-first")
	require.Eventually(t, func() bool { return reviews.Load() == 1 }, 5*time.Second, time.Millisecond)
	require.NoError(t, client.reset(first))
	client.ping()
	require.False(t, firstAnswered.Load(), "the review must still be in flight when the server has handled the reset")
	releaseReview()
	require.LessOrEqual(t, fixture.settledFailureTokens(t), float64(limits.AuthFailureBurst-1)+limits.AuthFailureRate*time.Since(started).Seconds(),
		"the stream reset during its review must be charged")

	const streams = 600
	slots := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for i := range streams {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			token := fmt.Sprintf("rejected-%d", i)
			id := client.openGRPC(ecdsStreamMethod, token)
			// Reset once the review has started or the request was refused,
			// so every request is counted when the loop ends.
			assert.Eventually(t, func() bool {
				if _, ok := reviewing.Load(token); ok {
					return true
				}
				select {
				case <-client.channel(id):
					return true
				default:
					return false
				}
			}, 5*time.Second, time.Millisecond, "stream %d was neither reviewed nor refused", id)
			_ = client.reset(id)
		})
	}
	wg.Wait()
	// Reviews still running after their request ended finish before the
	// sample.
	fixture.settledFailureTokens(t)
	elapsed := time.Since(started)
	allowed := int64(limits.AuthFailureBurst) + int64(limits.AuthFailureRate*elapsed.Seconds()) + 1
	t.Logf("%d garbage-token streams reset mid-review in %v: %d TokenReviews (allowed at most %d), %v throttled",
		streams, elapsed, reviews.Load(), allowed, testutil.ToFloat64(fixture.metrics.authThrottled))
	require.LessOrEqual(t, reviews.Load(), allowed)
	require.Greater(t, reviews.Load(), int64(limits.AuthFailureBurst/2), "the budget must have been spent on reviews")
}

// settledFailureTokens waits until no TokenReview from 127.0.0.1 is in flight
// and returns the tokens left in its failure bucket.
func (f *listenerFixture) settledFailureTokens(t *testing.T) float64 {
	t.Helper()
	limiter := f.server.Auth.failureLimiter
	var tokens float64
	require.Eventually(t, func() bool {
		limiter.mu.Lock()
		defer limiter.mu.Unlock()
		bucket := limiter.buckets["127.0.0.1"]
		if bucket == nil || bucket.pending != 0 {
			return false
		}
		tokens = bucket.limiter.TokensAt(time.Now())
		return true
	}, 5*time.Second, time.Millisecond)
	return tokens
}

// windowUpdate gives stream id and its connection size bytes of receive window
// once.
func (c *rawH2) windowUpdate(id uint32, size uint32) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.framer.WriteWindowUpdate(id, size); err != nil {
		return err
	}
	return c.framer.WriteWindowUpdate(0, size)
}

// grantWindow gives stream id and its connection size bytes of receive window
// every interval until stop closes, so the server can send at that pace.
func (c *rawH2) grantWindow(id uint32, size uint32, interval time.Duration, stop <-chan struct{}) {
	go func() {
		for {
			_ = c.windowUpdate(id, size)
			select {
			case <-stop:
				return
			case <-time.After(interval):
			}
		}
	}()
}

// A stream whose handler ends while its response is still being written is
// reset at once and gives back its slots, instead of keeping the response in
// flight until the write timeout.
func TestListenerEndsResponseInFlightWhenStreamEnds(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.largeSnapshot(t, "v1", 8<<20)))
	client := dialRawH2(t, fixture.listenerFixture, 0) // the response can never be sent

	id := client.openGRPC(ecdsStreamMethod, validToken)
	client.data(id, grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}}), h2MaxReadFrameSize)
	require.Eventually(t, func() bool { return fixture.registeredStreams() == 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	// An invalid request ends the stream's handler.
	invalid := &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName, fixture.resourceName + "-other"}}
	client.data(id, grpcFrame(t, invalid), h2MaxReadFrameSize)
	started := time.Now()
	result := client.wait(id, 5*time.Second)
	fixture.requireNoStreams(t)
	t.Logf("the in-flight response was reset %v after the stream ended; slots free after %v", result.at.Sub(started), time.Since(started))
	require.True(t, result.reset)
	require.Less(t, time.Since(started), 2*time.Second)
}

// A revoked stream stops receiving the response it is being sent, also when
// another response waits behind it.
func TestListenerRevocationEndsResponseInFlight(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%t", queued), func(t *testing.T) {
			limits := DefaultLimits()
			limits.StreamMaxLifetime = time.Hour
			// Far beyond the test, so only the revocation can end the write.
			limits.WriteTimeout = time.Minute
			fixture := newStreamFixtureWithLimits(t, limits)
			require.NoError(t, fixture.store.Replace(fixture.largeSnapshot(t, "v1", 4<<20)))
			client := dialRawH2(t, fixture.listenerFixture, 0)
			id := client.openGRPC(ecdsStreamMethod, validToken)
			client.data(id, grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}}), h2MaxReadFrameSize)
			stop := make(chan struct{})
			defer close(stop)
			// A slow reader: 4 MiB at 256 KiB per 0.8s takes about 13s.
			client.grantWindow(id, 256<<10+64, 800*time.Millisecond, stop)
			require.Eventually(t, func() bool { return client.received.Load() > 0 }, 5*time.Second, 10*time.Millisecond)
			if queued {
				// Acknowledge v1 so a watch opens, then publish v2: the watch
				// answers and v2 waits behind v1.
				node := testNamespace + "/" + streamEngine
				watches := func() int64 {
					if info := fixture.store.cache.GetStatusInfo(node); info != nil {
						return int64(info.GetNumWatches())
					}
					return 0
				}
				client.data(id, grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}, VersionInfo: "sha256:v1", ResponseNonce: "1"}), h2MaxReadFrameSize)
				require.Eventually(t, func() bool { return watches() == 1 }, 5*time.Second, time.Millisecond, "the acknowledgement must open a watch")
				require.NoError(t, fixture.store.Replace(fixture.largeSnapshot(t, "v2", 4<<20)))
				// The server records a response just before sending it, so v2
				// is on its way to the stream, behind v1 still being written.
				sending := func() string {
					var version string
					fixture.server.streams.Range(func(_, value any) bool {
						entry := value.(*serverStream)
						entry.mu.Lock()
						version = entry.lastVersion
						entry.mu.Unlock()
						return false
					})
					return version
				}
				require.Eventually(t, func() bool { return sending() == "sha256:v2" }, 5*time.Second, time.Millisecond, "v2 must reach the send path")
				require.NotZero(t, fixture.responseWriting(), "v1 must still be being written")
				require.Less(t, client.received.Load(), int64(4<<20), "v1 must still be being written")
			}

			revoked := time.Now()
			fixture.authz.RevokeAndNotify(fixture.resourceName)
			result := client.wait(id, 5*time.Second)
			fixture.requireNoStreams(t)
			t.Logf("revoked stream ended %v after revocation (reset %t) having received %d KiB of 4096 KiB",
				result.at.Sub(revoked), result.reset, client.received.Load()>>10)
			require.True(t, result.reset, "the response in flight must be reset")
			require.False(t, result.at.Before(revoked), "the stream must not end before the revocation")
			require.Less(t, result.at.Sub(revoked), time.Second)
			require.Zero(t, testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalWriteTimeout)), "a revocation is not a write timeout")
		})
	}
}

// One response must be written within the write timeout plus its size at the
// minimum rate, even when every chunk is read just in time.
func TestListenerBoundsResponseWriteTime(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	limits.WriteTimeout = 1500 * time.Millisecond
	fixture := newStreamFixtureWithLimits(t, limits)
	fixture.listener.minWriteRate = 1 << 20 // 2 MiB: 1.5s plus 2s
	const size = 2 << 20
	require.NoError(t, fixture.store.Replace(fixture.largeSnapshot(t, "v1", size)))
	client := dialRawH2(t, fixture.listenerFixture, 0)
	id := client.openGRPC(ecdsStreamMethod, validToken)
	client.data(id, grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}}), h2MaxReadFrameSize)
	stop := make(chan struct{})
	defer close(stop)
	// Each 256 KiB chunk arrives within its 1.5s deadline; the whole response
	// needs nine grants and would take over 5s.
	started := time.Now()
	client.grantWindow(id, 256<<10, 650*time.Millisecond, stop)
	result := client.wait(id, 10*time.Second)
	t.Logf("a response read at about 400 KiB/s was reset after %v having received %d KiB", result.at.Sub(started), client.received.Load()>>10)
	require.True(t, result.reset)
	// The reset comes at the whole response's bound, not at a chunk deadline:
	// it cannot come earlier, while a loaded host can make it later.
	whole := started.Add(limits.WriteTimeout + size*time.Second/time.Duration(fixture.listener.minWriteRate))
	require.WithinRange(t, result.at, whole.Add(-250*time.Millisecond), whole.Add(time.Second),
		"the response must be reset at its whole bound")
	require.Greater(t, client.received.Load(), int64(3*writeChunk), "the response must have made progress past its first chunk deadlines")
}

// The body reads at most messagesAhead messages ahead of the handler, one
// more for each the handler receives, and closing it wakes a waiting read.
func TestRequestBodyReadsAtMostMessagesAhead(t *testing.T) {
	message := grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{"resource"}})
	body := &requestBody{
		ReadCloser: io.NopCloser(bytes.NewReader(bytes.Repeat(message, 2*messagesAhead))),
		controller: http.NewResponseController(httptest.NewRecorder()),
		timeout:    time.Second,
		maxBytes:   1 << 20,
		ahead:      make(chan struct{}, messagesAhead),
		closed:     make(chan struct{}),
	}
	var read atomic.Int64
	ended := make(chan error, 1)
	go func() {
		buf := make([]byte, 16<<10)
		for {
			n, err := body.Read(buf)
			read.Add(int64(n))
			if err != nil {
				ended <- err
				return
			}
		}
	}()
	messages := func() int64 { return read.Load() / int64(len(message)) }
	require.Eventually(t, func() bool { return messages() == messagesAhead }, 5*time.Second, time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.EqualValues(t, messagesAhead, messages(), "reads must stop at the limit")

	body.received()
	require.Eventually(t, func() bool { return messages() == messagesAhead+1 }, 5*time.Second, time.Millisecond)

	require.NoError(t, body.Close())
	select {
	case err := <-ended:
		require.ErrorIs(t, err, http.ErrBodyReadAfterClose)
	case <-time.After(5 * time.Second):
		t.Fatal("closing the body did not wake the waiting read")
	}
}

// A stream whose handler is paused, here behind a second response to a peer
// that reads none, keeps the request messages beyond messagesAhead in the
// peer's flow-control window instead of the server's heap.
func TestListenerBoundsMessagesAheadOfHandler(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	limits.WriteTimeout = time.Minute
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	client := dialRawH2(t, fixture.listenerFixture, 0) // no response is ever sent
	id := client.openGRPC(ecdsStreamMethod, validToken)
	client.data(id, grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}}), h2MaxReadFrameSize)
	require.Eventually(t, func() bool { return fixture.registeredStreams() == 1 }, 5*time.Second, 10*time.Millisecond)
	// gRPC hands the first response over without waiting for it to be
	// written; the handler blocks sending the second.
	ack := grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}, VersionInfo: "sha256:v1", ResponseNonce: "1"})
	client.data(id, ack, h2MaxReadFrameSize)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	time.Sleep(200 * time.Millisecond)

	const messages = 1000 // within the stream's receive window
	before := heapBytes()
	client.data(id, bytes.Repeat(ack, messages), h2MaxReadFrameSize)
	time.Sleep(300 * time.Millisecond)
	held := heapGrowth(before, heapBytes())
	t.Logf("%d messages sent to a paused handler: heap growth %d KiB", messages, held>>10)
	// Unbounded, grpc-go keeps a 16 KiB buffer per message: about 16 MiB here.
	require.Less(t, held, uint64(4<<20))
	select {
	case result := <-client.channel(id):
		t.Fatalf("the stream ended: %+v", result)
	default:
	}
}

// Requests grpc-go would refuse, another method or media type, are refused
// before any TokenReview; the gRPC media type with a subtype is reviewed.
func TestListenerRefusesNonGRPCRequestsBeforeReview(t *testing.T) {
	fixture := newListenerFixture(t)
	var reviews atomic.Int32
	fixture.server.Auth.tokenReviews = reviewFunc(func(context.Context, *authv1.TokenReview) (*authv1.TokenReview, error) {
		reviews.Add(1)
		return rejectedReview(), nil
	})
	client := dialRawH2(t, fixture, 65535)
	refused := []struct {
		method, contentType string
		headers             []string
	}{
		{http.MethodGet, "application/grpc", nil},
		{http.MethodPost, "application/grpcX", nil},
		{http.MethodPost, "application/grpc-web", nil},
		{http.MethodPost, grpcContentType, []string{"grpc-timeout", "bogus"}},
		{http.MethodPost, grpcContentType, []string{"grpc-timeout", "123456789S"}},
		{http.MethodPost, grpcContentType, []string{"grpc-timeout", "5x"}},
		{http.MethodPost, grpcContentType, []string{"x-key-bin", "!!!"}},
	}
	for _, request := range refused {
		headers := append([]string{"authorization", "Bearer " + validToken}, request.headers...)
		id := client.open(request.method, ecdsStreamMethod, request.contentType, true, headers...)
		require.Equal(t, strconv.Itoa(int(codes.Unimplemented)), client.wait(id, 5*time.Second).grpcStatus, "%s %s %v", request.method, request.contentType, request.headers)
	}
	require.Zero(t, reviews.Load())
	require.Equal(t, float64(len(refused)), testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalMethod)))

	// A subtype, a valid timeout and binary metadata, padded or not, pass.
	for i, request := range []struct{ contentType, name, value string }{
		{"application/grpc+proto", "grpc-timeout", "8S"},
		{grpcContentType, "x-key-bin", "AQID"},
		{grpcContentType, "x-key-bin", "AQI"},
	} {
		id := client.open(http.MethodPost, ecdsStreamMethod, request.contentType, false, "authorization", "Bearer "+validToken, request.name, request.value)
		require.Equal(t, strconv.Itoa(int(codes.Unauthenticated)), client.wait(id, 5*time.Second).grpcStatus, "%v", request)
		require.EqualValues(t, i+1, reviews.Load())
	}
}

// deadlineRecorder fails writes once its write deadline has passed.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.deadline = deadline
	return nil
}

func (r *deadlineRecorder) Write(p []byte) (int, error) {
	if !r.deadline.IsZero() && !time.Now().Before(r.deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	return r.ResponseRecorder.Write(p)
}

// A write that abort ends is not a write timeout; one that misses its own
// deadline is.
func TestDeadlineWriterCountsOnlyMissedDeadlines(t *testing.T) {
	for _, tt := range []struct {
		name    string
		timeout time.Duration
		abort   bool
		counted float64
	}{
		{"aborted", time.Minute, true, 0},
		{"missed deadline", -time.Second, false, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metrics, err := NewMetrics(prometheus.NewRegistry())
			require.NoError(t, err)
			recorder := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			writer := &deadlineWriter{ResponseWriter: recorder, controller: http.NewResponseController(recorder), timeout: tt.timeout, metrics: metrics}
			if tt.abort {
				writer.abort()
			}
			_, err = writer.Write(make([]byte, 100))
			require.ErrorIs(t, err, os.ErrDeadlineExceeded)
			require.Equal(t, tt.counted, testutil.ToFloat64(metrics.refusals.WithLabelValues(RefusalWriteTimeout)))
		})
	}
}

// responseWriting returns the highest ID of a stream whose response is being
// written or flushed, or zero.
func (f *listenerFixture) responseWriting() int64 {
	var newest int64
	f.server.streams.Range(func(_, value any) bool {
		entry := value.(*serverStream)
		if entry.admission != nil && entry.admission.writer != nil {
			writer := entry.admission.writer
			writer.mu.Lock()
			if writer.writing {
				newest = max(newest, entry.streamID)
			}
			writer.mu.Unlock()
		}
		return true
	})
	return newest
}

// awaitWritten waits until no response is being written, so a stream closed
// next ends with its status rather than a reset of a response in flight.
func (f *listenerFixture) awaitWritten(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return f.responseWriting() == 0 }, 5*time.Second, time.Millisecond)
}

// A revoked stream whose small response waits in its flush for flow-control
// credit is reset at once, and the response is not sent when credit comes.
func TestListenerRevocationEndsFlushInFlight(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	limits.WriteTimeout = time.Minute
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	client := dialRawH2(t, fixture.listenerFixture, 0) // the response waits for credit
	id := client.openGRPC(ecdsStreamMethod, validToken)
	client.data(id, grpcFrame(t, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}}), h2MaxReadFrameSize)
	require.Eventually(t, func() bool { return fixture.responseWriting() != 0 }, 5*time.Second, time.Millisecond)

	revoked := time.Now()
	fixture.authz.RevokeAndNotify(fixture.resourceName)
	result := client.wait(id, 5*time.Second)
	t.Logf("a revoked stream with a response waiting for credit ended %v after revocation (reset %t)", result.at.Sub(revoked), result.reset)
	require.True(t, result.reset)
	require.Less(t, result.at.Sub(revoked), time.Second)
	fixture.requireNoStreams(t)

	require.NoError(t, client.windowUpdate(id, 1<<20))
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, client.received.Load(), "the revoked response must not be sent")
}
