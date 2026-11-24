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
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

type listenerFixture struct {
	*serverStack
	listener *Listener
	cancel   context.CancelFunc
	pool     *x509.CertPool
	manifest *assets.Manifest
}

func newListenerFixture(t *testing.T) *listenerFixture {
	return newListenerFixtureWithLimits(t, DefaultLimits())
}

func newListenerFixtureWithLimits(t *testing.T, limits Limits) *listenerFixture {
	t.Helper()
	// Certificates are generated per run so no private key lives in the repo.
	dir := t.TempDir()
	ca, caKey, caPEM := newTestCA(t)
	_, certPEM, keyPEM := issueTestLeaf(t, ca, caKey, 1)
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	return newListenerFixtureWithConfig(t, ListenerConfig{
		BindAddress: "127.0.0.1:0",
		CertDir:     dir,
		CertName:    "tls.crt",
		KeyName:     "tls.key",
		CAPath:      filepath.Join(dir, "ca.crt"),
	}, limits)
}

func newListenerFixtureWithConfig(t *testing.T, config ListenerConfig, limits Limits) *listenerFixture {
	t.Helper()
	// Every test client dials from loopback, so one source address stands for
	// many peers unless a test sets its own share.
	if limits.MaxConnectionsPerSource == 0 {
		limits.MaxConnectionsPerSource = 1024
	}

	manifest, data, err := assets.ResolveDefault()
	if err != nil {
		t.Fatal(err)
	}
	stack := newServerStack(t, limits)
	listener, err := NewListener(config, stack.server, NewAssetHandler(manifest, data, stack.metrics, limits.WithDefaults().WriteTimeout), limits, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- listener.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Listener.Start() error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("listener did not stop")
		}
	})

	select {
	case <-listener.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not bind")
	}

	caPEM, err := os.ReadFile(config.CAPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to load test CA")
	}

	return &listenerFixture{serverStack: stack, listener: listener, cancel: cancel, pool: pool, manifest: manifest}
}

func TestListenerALPNDispatch(t *testing.T) {
	fixture := newListenerFixture(t)
	address := fixture.listener.Addr().String()

	t.Run("h2 reaches gRPC", func(t *testing.T) {
		creds := credentials.NewTLS(&tls.Config{RootCAs: fixture.pool, ServerName: "localhost"})
		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(creds))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()

		stream, err := extensionv3.NewExtensionConfigDiscoveryServiceClient(conn).StreamExtensionConfigs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		send(t, stream, &discoveryv3.DiscoveryRequest{ResourceNames: []string{ResourceName("shop", "checkout", "uid")}})
		_, err = stream.Recv()
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("Recv() code = %v, want Unauthenticated (h2 must reach gRPC)", status.Code(err))
		}
	})

	t.Run("h2 reaches assets too", func(t *testing.T) {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: fixture.pool, ServerName: "localhost"}, ForceAttemptHTTP2: true}
		client := &http.Client{Transport: transport}

		response, err := client.Get("https://" + address + assetPathPrefix + fixture.manifest.SHA256 + assetPathSuffix)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.ProtoMajor != 2 {
			t.Fatalf("protocol = %s, want HTTP/2", response.Proto)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("asset status = %d, want 200", response.StatusCode)
		}
		if len(body) == 0 {
			t.Fatal("asset body is empty")
		}
	})

	t.Run("http/1.1 reaches assets, not /rules", func(t *testing.T) {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: fixture.pool, ServerName: "localhost"}, ForceAttemptHTTP2: false}
		client := &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}

		response, err := client.Get("https://" + address + assetPathPrefix + fixture.manifest.SHA256 + assetPathSuffix)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("asset status = %d, want 200", response.StatusCode)
		}
		if len(body) == 0 {
			t.Fatal("asset body is empty")
		}

		rules, err := client.Get("https://" + address + "/rules/shop/checkout")
		if err != nil {
			t.Fatal(err)
		}
		_ = rules.Body.Close()
		if rules.StatusCode != http.StatusNotFound {
			t.Fatalf("/rules status = %d, want 404", rules.StatusCode)
		}
	})
}

func TestListenerNegotiatesALPN(t *testing.T) {
	fixture := newListenerFixture(t)
	address := fixture.listener.Addr().String()

	for _, protocol := range []string{"h2", "http/1.1"} {
		t.Run(protocol, func(t *testing.T) {
			connection, err := tls.Dial("tcp", address, &tls.Config{
				RootCAs:    fixture.pool,
				ServerName: "localhost",
				NextProtos: []string{protocol},
				MinVersion: tls.VersionTLS13,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close() }()
			if got := connection.ConnectionState().NegotiatedProtocol; got != protocol {
				t.Fatalf("negotiated protocol = %q, want %q", got, protocol)
			}
		})
	}
}

func TestListenerH2RejectsOversizedHeaders(t *testing.T) {
	fixture := newListenerFixture(t)
	address := fixture.listener.Addr().String()

	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: fixture.pool, ServerName: "localhost"},
		ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	request, err := http.NewRequest(http.MethodGet, "https://"+address+assetPathPrefix+fixture.manifest.SHA256+assetPathSuffix, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Oversized", strings.Repeat("a", 64<<10))

	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Fatalf("oversized header accepted over h2: status %d", response.StatusCode)
	}
}

func TestListenerReady(t *testing.T) {
	fixture := newListenerFixture(t)
	if err := fixture.listener.Ready(); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
	// The Engine reconciler reads this same source, so a CA loaded after
	// startup cannot be visible to only one of the two paths.
	if err := fixture.listener.CASource().Ready(); err != nil {
		t.Fatalf("shared CA source is not ready: %v", err)
	}
}

// newTestCA mints a self-signed CA; issueTestLeaf mints a leaf signed by it.
func newTestCA(t *testing.T) (*x509.Certificate, ed25519.PrivateKey, []byte) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func issueTestLeaf(t *testing.T, ca *x509.Certificate, caKey ed25519.PrivateKey, serial int64) (*x509.Certificate, []byte, []byte) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return leaf,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// TestListenerMissingCertificatesStayUpAndRecover checks the startup
// contract: a missing serving certificate must not stop the listener, and
// readiness plus TLS serving must recover once the mounted Secret appears.
func TestListenerMissingCertificatesStayUpAndRecover(t *testing.T) {
	dir := t.TempDir()
	ca, caKey, caPEM := newTestCA(t)
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), caPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	fixture := newListenerFixtureWithConfig(t, ListenerConfig{
		BindAddress: "127.0.0.1:0",
		CertDir:     dir,
		CertName:    "tls.crt",
		KeyName:     "tls.key",
		CAPath:      filepath.Join(dir, "ca.crt"),
	}, DefaultLimits())

	if err := fixture.listener.Ready(); err == nil {
		t.Fatal("Ready() must fail while the serving certificate is missing")
	}

	leaf, certPEM, keyPEM := issueTestLeaf(t, ca, caKey, 2)
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for err := fixture.listener.Ready(); err != nil; err = fixture.listener.Ready() {
		if time.Now().After(deadline) {
			t.Fatalf("readiness must recover once the certificate appears: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	connection, err := tls.Dial("tcp", fixture.listener.Addr().String(), &tls.Config{
		RootCAs:    fixture.pool,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	if !connection.ConnectionState().PeerCertificates[0].Equal(leaf) {
		t.Fatal("the certificate that appeared after startup must be served")
	}
}

// TestListenerRotatesCertificate checks that rewriting the certificate files
// swaps the served certificate without a restart.
func TestListenerRotatesCertificate(t *testing.T) {
	dir := t.TempDir()
	ca, caKey, caPEM := newTestCA(t)
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	_, certPEM, keyPEM := issueTestLeaf(t, ca, caKey, 1)
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	fixture := newListenerFixtureWithConfig(t, ListenerConfig{
		BindAddress: "127.0.0.1:0",
		CertDir:     dir,
		CertName:    "tls.crt",
		KeyName:     "tls.key",
		CAPath:      filepath.Join(dir, "ca.crt"),
	}, DefaultLimits())
	address := fixture.listener.Addr().String()

	dial := func() *x509.Certificate {
		connection, err := tls.Dial("tcp", address, &tls.Config{
			RootCAs:    fixture.pool,
			ServerName: "localhost",
			MinVersion: tls.VersionTLS13,
		})
		if err != nil {
			return nil
		}
		defer func() { _ = connection.Close() }()
		return connection.ConnectionState().PeerCertificates[0]
	}

	if leaf := dial(); leaf == nil {
		t.Fatal("TLS handshake failed with the initial certificate")
	}

	nextLeaf, nextCertPEM, nextKeyPEM := issueTestLeaf(t, ca, caKey, 2)
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), nextCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), nextKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	// fsnotify picks the rewrite up promptly; the watcher's interval tick is
	// the backstop.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if leaf := dial(); leaf != nil && leaf.Equal(nextLeaf) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the rotated certificate was not served")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// requireLocalAddress skips the test when address is not a local address,
// as on hosts where loopback is only 127.0.0.1.
func requireLocalAddress(t *testing.T, address string) {
	t.Helper()
	probe, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Skipf("%s is not a local address: %v", address, err)
	}
	_ = probe.Close()
}

// dialHTTP1 completes a TLS handshake for HTTP/1.1 from the local address
// source; empty means any.
func (f *listenerFixture) dialHTTP1(source string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	if source != "" {
		dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(source)}
	}
	return tls.DialWithDialer(dialer, "tcp", f.listener.Addr().String(), &tls.Config{
		RootCAs: f.pool, ServerName: "localhost", NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS13,
	})
}

// closedSoon reports whether the server closes conn within timeout.
func closedSoon(conn net.Conn, timeout time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_, err := conn.Read(make([]byte, 1))
	var netErr net.Error
	return err != nil && (!errors.As(err, &netErr) || !netErr.Timeout())
}

// A source over its share of connections without an admitted ECDS stream has
// its oldest one closed to make room; another source is not affected.
func TestListenerEvictsOldestConnectionPerSource(t *testing.T) {
	requireLocalAddress(t, "127.0.0.2")
	limits := DefaultLimits()
	limits.MaxConnectionsPerSource = 2
	fixture := newListenerFixtureWithLimits(t, limits)

	held := make([]net.Conn, 0, 3)
	for range 3 {
		conn, err := fixture.dialHTTP1("127.0.0.1")
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		held = append(held, conn)
	}
	require.True(t, closedSoon(held[0], 2*time.Second), "the oldest connection must make room for the newest")
	require.False(t, closedSoon(held[2], 200*time.Millisecond))
	other, err := fixture.dialHTTP1("127.0.0.2")
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	require.False(t, closedSoon(other, 200*time.Millisecond))
	require.Equal(t, float64(1), testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalConnectionEvicted)))
}

// Connections that carry an admitted ECDS stream are never closed to make
// room; a flood of connections without one only displaces its own.
func TestListenerKeepsAdmittedConnectionsUnderFlood(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStreams = 2 // and at most 2 connections without an admitted stream
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	ctx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+validToken))
	defer cancel()
	streams := make([]extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient, 0, limits.MaxStreams)
	for range limits.MaxStreams {
		stream, err := fixture.newECDSClient(t).StreamExtensionConfigs(ctx)
		require.NoError(t, err)
		send(t, stream, &discoveryv3.DiscoveryRequest{ResourceNames: []string{fixture.resourceName}})
		first := recv(t, stream)
		send(t, stream, &discoveryv3.DiscoveryRequest{
			ResourceNames: []string{fixture.resourceName}, VersionInfo: first.GetVersionInfo(), ResponseNonce: first.GetNonce(),
		})
		streams = append(streams, stream)
	}

	for range 20 {
		conn, err := fixture.dialHTTP1("")
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
	}
	require.GreaterOrEqual(t, testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalConnectionEvicted)), float64(18))

	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	for _, stream := range streams {
		require.Equal(t, fixture.wire("v2", 0), recv(t, stream).GetVersionInfo())
	}
}

// An HTTP/2 connection that starts no request is closed after the idle
// timeout, while a connection carrying an idle ECDS stream is kept.
func TestListenerClosesConnectionWithoutStream(t *testing.T) {
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	fixture := newStreamFixtureWithLimits(t, limits)
	fixture.listener.h2.IdleTimeout = 200 * time.Millisecond
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	stream := fixture.open(t, validToken, fixture.resourceName)
	first := recv(t, stream)
	send(t, stream, &discoveryv3.DiscoveryRequest{
		ResourceNames: []string{fixture.resourceName}, VersionInfo: first.GetVersionInfo(), ResponseNonce: first.GetNonce(),
	})

	idle, err := tls.Dial("tcp", fixture.listener.Addr().String(), &tls.Config{
		RootCAs: fixture.pool, ServerName: "localhost", NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS13,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = idle.Close() })
	_, err = idle.Write([]byte(http2.ClientPreface))
	require.NoError(t, err)
	require.NoError(t, http2.NewFramer(idle, idle).WriteSettings())
	started := time.Now()
	_ = idle.SetReadDeadline(started.Add(5 * time.Second))
	_, err = io.Copy(io.Discard, idle)
	t.Logf("an HTTP/2 connection without a stream was closed after %v", time.Since(started))
	require.NoError(t, err, "the server must close the connection")

	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v2")))
	require.Equal(t, fixture.wire("v2", 0), recv(t, stream).GetVersionInfo())
}

// The module downloads in full over HTTP/2 and HTTP/1.1.
func TestListenerServesModuleDownload(t *testing.T) {
	fixture := newListenerFixture(t)
	const size = 16 << 20
	asset := serveLargeAsset(fixture, size)
	for _, h2 := range []bool{true, false} {
		require.NoError(t, fixture.download(asset, size, h2, "", 0), "HTTP/2 %t", h2)
	}
}

// download fetches path over HTTP/2 or HTTP/1.1 from the local address source,
// empty for any, reading at most bytesPerSecond unless it is zero, and checks
// that size bytes arrive over that protocol.
func (f *listenerFixture) download(path string, size int, h2 bool, source string, bytesPerSecond int) error {
	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: f.pool, ServerName: "localhost"},
		ForceAttemptHTTP2: h2,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialer := &net.Dialer{}
			if source != "" {
				dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(source)}
			}
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil || bytesPerSecond == 0 {
				return conn, err
			}
			return &throttledConn{Conn: conn, bytesPerSecond: bytesPerSecond}, nil
		},
	}
	defer transport.CloseIdleConnections()
	// The timeout covers the headers and the whole body, so a stalled
	// listener fails the download instead of hanging the test.
	response, err := (&http.Client{Transport: transport, Timeout: 2 * time.Minute}).Get("https://" + f.listener.Addr().String() + path)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err == nil && (response.StatusCode != http.StatusOK || len(body) != size || (response.ProtoMajor == 2) != h2) {
		err = fmt.Errorf("status %d with %d bytes over HTTP/%d", response.StatusCode, len(body), response.ProtoMajor)
	}
	return err
}

// floodConnections holds connections without a token from source until stop
// closes: handshake-less TCP, idle TLS, or HTTP/1.1 keep-alive with a request
// a second, dialling again whenever the server closes one.
func floodConnections(fixture *listenerFixture, source string, kind int, stop <-chan struct{}) {
	dialer := &net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}}
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	for !stopped() {
		var conn net.Conn
		var err error
		switch kind {
		case 0:
			conn, err = dialer.Dial("tcp", fixture.listener.Addr().String())
		default:
			conn, err = fixture.dialHTTP1(source)
		}
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		for !stopped() {
			if kind == 2 {
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				if _, err := conn.Write([]byte("GET /x HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
					break
				}
				response, err := http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					break
				}
				_ = response.Body.Close()
			}
			if closedSoon(conn, time.Second) {
				break
			}
		}
		_ = conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
}

// Peers without a token flooding handshake-less, idle and HTTP/1.1 keep-alive
// connections from 32 source addresses cannot stop a module download over
// either protocol or a Gateway whose token review takes 50ms.
func TestListenerKeepsGatewaysReachableUnderConnectionFlood(t *testing.T) {
	// The flood comes from 127.0.0.2-33; each download from an address of its
	// own, as each Gateway pod has one, and the Gateway from 127.0.0.1.
	sources := make([]string, 0, 34)
	for i := 2; i < 36; i++ {
		source := fmt.Sprintf("127.0.0.%d", i)
		requireLocalAddress(t, source)
		sources = append(sources, source)
	}
	sources, downloaders := sources[:32], sources[32:]
	limits := DefaultLimits()
	limits.StreamMaxLifetime = time.Hour
	// 64 connections without an admitted stream, 4 per source by default: the
	// flood holds twice the pool, two per source, as at the default limits
	// with a larger flood. Fairness holds while the flood has fewer source
	// addresses than the pool holds connections.
	limits.MaxStreams = 64
	limits.MaxConnectionsPerSource = limits.WithDefaults().MaxConnectionsPerSource
	const perSource = 4
	fixture := newStreamFixtureWithLimits(t, limits)
	require.NoError(t, fixture.store.Replace(fixture.snapshot(t, "v1")))
	fixture.setTokenDelay(50 * time.Millisecond)
	const size = 2 << 20
	asset := serveLargeAsset(fixture.listenerFixture, size)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i, source := range sources {
		for range perSource {
			wg.Go(func() { floodConnections(fixture.listenerFixture, source, i%3, stop) })
		}
	}
	defer func() { close(stop); wg.Wait() }()
	evicted := func() float64 {
		return testutil.ToFloat64(fixture.metrics.refusals.WithLabelValues(RefusalConnectionEvicted))
	}
	require.Eventually(t, func() bool { return evicted() >= float64(len(sources)*perSource) }, 10*time.Second, 10*time.Millisecond)

	// Each download is read at 2 MiB/s, so it spends about a second under the
	// flood.
	started, before := time.Now(), evicted()
	errs := make([]error, 3)
	var clients sync.WaitGroup
	clients.Go(func() { errs[0] = fixture.download(asset, size, true, downloaders[0], 2<<20) })
	clients.Go(func() { errs[1] = fixture.download(asset, size, false, downloaders[1], 2<<20) })
	clients.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		errs[2] = fixture.subscribe(ctx, fixture.newECDSClient(t), podToken("gateway-pod"))
	})
	clients.Wait()
	elapsed := time.Since(started)
	t.Logf("two 2 MiB downloads and a Gateway with a 50ms token review finished in %v under a flood from %d sources closing %.0f connections/s",
		elapsed, len(sources), (evicted()-before)/elapsed.Seconds())
	require.NoError(t, errs[0], "HTTP/2 download")
	require.NoError(t, errs[1], "HTTP/1.1 download")
	require.NoError(t, errs[2], "Gateway")
}

// A newcomer over the pool displaces a connection of the source holding the
// most, never one of a smaller source; within it, one without a request goes
// before one with a request in progress, and a protected one last. Over its
// share a source displaces its own.
func TestConnectionPoolEvictsFairly(t *testing.T) {
	type held struct {
		source              string
		requests, protected int
	}
	tests := []struct {
		name                        string
		maxUnadmitted, maxPerSource int
		held                        []held // oldest first
		newcomer                    string
		evicted                     []int // indexes into held; len(held) is the newcomer
	}{
		{"the source holding the most, not an older smaller one", 3, 8,
			[]held{{source: "b"}, {source: "a"}, {source: "a"}}, "c", []int{1}},
		{"no request before one in progress", 3, 8,
			[]held{{source: "a", requests: 1}, {source: "a"}, {source: "b"}}, "c", []int{1}},
		{"in progress before protected", 3, 8,
			[]held{{source: "a", requests: 1, protected: 1}, {source: "a", requests: 1}, {source: "b"}}, "c", []int{1}},
		{"protected when nothing else is left", 2, 8,
			[]held{{source: "a", requests: 1, protected: 1}, {source: "a", requests: 1, protected: 1}}, "c", []int{0}},
		{"ties go to the least busy, then the oldest", 3, 8,
			[]held{{source: "a", requests: 1}, {source: "b"}, {source: "c"}}, "d", []int{1}},
		{"over its share a source displaces its own", 8, 2,
			[]held{{source: "a", requests: 1}, {source: "b"}, {source: "a"}}, "a", []int{2}},
		{"a busy source refuses its own newcomer", 8, 2,
			[]held{{source: "a", requests: 1}, {source: "a", requests: 1, protected: 1}, {source: "b"}}, "a", []int{3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := &connectionPool{maxUnadmitted: tt.maxUnadmitted, maxPerSource: tt.maxPerSource, perSource: map[string]int{}}
			conns := make([]*trackedConn, 0, len(tt.held)+1)
			for _, h := range tt.held {
				c := &trackedConn{pool: pool, source: h.source, requests: h.requests, protected: h.protected}
				require.Empty(t, pool.add(c))
				conns = append(conns, c)
			}
			conns = append(conns, &trackedConn{pool: pool, source: tt.newcomer})
			got := []int{}
			for _, c := range pool.add(conns[len(conns)-1]) {
				got = append(got, slices.Index(conns, c))
			}
			require.Equal(t, tt.evicted, got)
		})
	}
}

// rankRecorder records its connection's rank when the response starts.
type rankRecorder struct {
	*httptest.ResponseRecorder
	conn *trackedConn
	rank int
}

func (r *rankRecorder) WriteHeader(code int) {
	r.rank = r.conn.rankLocked()
	r.ResponseRecorder.WriteHeader(code)
}

// Only a GET of the published module protects its connection, and only while
// its response is written: a request for another digest does not, and a
// keep-alive connection is not protected once its download has finished.
func TestListenerProtectsModuleDownloadsWhileWritten(t *testing.T) {
	handler, manifest, _ := newAssetHandler(t)
	listener := &Listener{handler: handler}
	module := assetPathPrefix + manifest.SHA256 + assetPathSuffix
	tests := []struct {
		name, method, path string
		rank               int
	}{
		{"a GET of the published module", http.MethodGet, module, 2},
		{"a GET of an unpublished digest", http.MethodGet, assetPathPrefix + strings.Repeat("0", 64) + assetPathSuffix, 1},
		{"a HEAD of the published module", http.MethodHead, module, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &trackedConn{pool: &connectionPool{perSource: map[string]int{}}}
			recorder := &rankRecorder{ResponseRecorder: httptest.NewRecorder(), conn: conn}
			listener.serveAsset(conn, recorder, httptest.NewRequest(tt.method, tt.path, nil))
			require.Equal(t, tt.rank, recorder.rank, "while the response is written")
			require.Zero(t, conn.rankLocked(), "once the response is done")
		})
	}
}

// IPv6 sources share a per-source share by /64; IPv4 ones are keyed by host.
func TestConnectionSourceGroupsIPv6By64(t *testing.T) {
	for address, want := range map[string]string{
		"192.0.2.1:443":              "192.0.2.1",
		"[::ffff:192.0.2.1]:443":     "192.0.2.1",
		"[2001:db8:1:2:3::4]:443":    "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff::1]:443": "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:443":      "2001:db8:1:3::/64",
	} {
		addr, err := net.ResolveTCPAddr("tcp", address)
		require.NoError(t, err)
		require.Equal(t, want, connectionSource(addr), address)
	}
}

// A connection whose last admitted stream ends rejoins the pool under the same
// caps as a newcomer: over them another connection is closed for room, while
// with room it is kept.
func TestConnectionPoolCapsRejoiningConnection(t *testing.T) {
	for _, tt := range []struct {
		name          string
		maxUnadmitted int
		evicted       float64
	}{
		{"full pool", 2, 1},
		{"room", 3, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metrics, err := NewMetrics(prometheus.NewRegistry())
			require.NoError(t, err)
			pool := &connectionPool{maxUnadmitted: tt.maxUnadmitted, maxPerSource: 8, metrics: metrics, perSource: map[string]int{}}
			newConn := func(source string) *trackedConn {
				local, remote := net.Pipe()
				t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
				c := &trackedConn{Conn: local, pool: pool, source: source}
				require.Empty(t, pool.add(c))
				return c
			}
			returning := newConn("a")
			returning.admit()
			idle := []*trackedConn{newConn("b"), newConn("c")}

			returning.unadmit()
			pool.mu.Lock()
			defer pool.mu.Unlock()
			require.LessOrEqual(t, pool.unadmitted.Len(), tt.maxUnadmitted)
			require.False(t, returning.closed, "the returning connection is kept")
			require.Equal(t, tt.evicted == 1, idle[0].closed, "the oldest idle connection makes room")
			require.Equal(t, tt.evicted, testutil.ToFloat64(metrics.refusals.WithLabelValues(RefusalConnectionEvicted)))
		})
	}
}

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string   { return "temporary accept failure" }
func (temporaryAcceptError) Temporary() bool { return true }
func (temporaryAcceptError) Timeout() bool   { return false }

type scriptedListener struct {
	accept func() (net.Conn, error)
}

func (l *scriptedListener) Accept() (net.Conn, error) { return l.accept() }
func (l *scriptedListener) Close() error              { return nil }
func (l *scriptedListener) Addr() net.Addr            { return nil }

func TestAcceptBacksOffOnTemporaryErrors(t *testing.T) {
	var calls int32
	listener := &scriptedListener{accept: func() (net.Conn, error) {
		if atomic.AddInt32(&calls, 1) <= 3 {
			return nil, temporaryAcceptError{}
		}
		return nil, errors.New("permanent accept failure")
	}}
	l := &Listener{logger: logr.Discard()}

	err := l.accept(context.Background(), listener, nil)
	if err == nil || err.Error() != "permanent accept failure" {
		t.Fatalf("accept() error = %v, want the permanent error", err)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("Accept() calls = %d, want 4 (temporary errors retried)", got)
	}
}

func newAssetHandler(t *testing.T) (*AssetHandler, *assets.Manifest, []byte) {
	t.Helper()
	manifest, data, err := assets.ResolveDefault()
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return NewAssetHandler(manifest, data, metrics, DefaultWriteTimeout), manifest, data
}

func TestAssetHandlerServesManifestEntry(t *testing.T) {
	handler, manifest, data := newAssetHandler(t)

	request := httptest.NewRequest(http.MethodGet, assetPathPrefix+manifest.SHA256+assetPathSuffix, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != wasmContentType {
		t.Fatalf("Content-Type = %q, want %q", got, wasmContentType)
	}
	if got := recorder.Header().Get("Cache-Control"); got != immutableCacheMax {
		t.Fatalf("Cache-Control = %q, want %q", got, immutableCacheMax)
	}
	if recorder.Body.String() != string(data) {
		t.Fatal("body does not match the bundled module")
	}
}

func TestAssetHandlerServesRange(t *testing.T) {
	handler, manifest, data := newAssetHandler(t)

	request := httptest.NewRequest(http.MethodGet, assetPathPrefix+manifest.SHA256+assetPathSuffix, nil)
	request.Header.Set("Range", "bytes=0-3")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", recorder.Code)
	}
	if got := recorder.Body.String(); got != string(data[:4]) {
		t.Fatalf("body = %q, want first four module bytes", got)
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 0-3/"+strconv.Itoa(len(data)) {
		t.Fatalf("Content-Range = %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != wasmContentType {
		t.Fatalf("Content-Type = %q, want %q", got, wasmContentType)
	}
	if got := recorder.Header().Get("Cache-Control"); got != immutableCacheMax {
		t.Fatalf("Cache-Control = %q, want %q", got, immutableCacheMax)
	}
}

func TestAssetHandlerServesHead(t *testing.T) {
	handler, manifest, data := newAssetHandler(t)

	request := httptest.NewRequest(http.MethodHead, assetPathPrefix+manifest.SHA256+assetPathSuffix, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Body.Len(); got != 0 {
		t.Fatalf("HEAD body length = %d, want 0", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(len(data)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(data))
	}
}

func TestAssetHandlerRejectsUnknownAndMalformedPaths(t *testing.T) {
	handler, manifest, _ := newAssetHandler(t)

	validDigest := manifest.SHA256
	cases := map[string]string{
		"unknown digest": assetPathPrefix + "0000000000000000000000000000000000000000000000000000000000000000" + assetPathSuffix,
		"uppercase":      assetPathPrefix + "ABCDEF0000000000000000000000000000000000000000000000000000000000" + assetPathSuffix,
		"short digest":   assetPathPrefix + "abcd" + assetPathSuffix,
		"no suffix":      assetPathPrefix + validDigest,
		"plain name":     assetPathPrefix + "plugin.wasm",
		"legacy rules":   "/rules/shop/checkout",
		"directory":      assetPathPrefix,
		"traversal":      assetPathPrefix + "..%2f" + validDigest + assetPathSuffix,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", recorder.Code)
			}
			if location := recorder.Header().Get("Location"); location != "" {
				t.Fatalf("Location = %q, want no redirect", location)
			}
		})
	}
}

func TestAssetHandlerRejectsNonGet(t *testing.T) {
	handler, manifest, _ := newAssetHandler(t)
	request := httptest.NewRequest(http.MethodPost, assetPathPrefix+manifest.SHA256+assetPathSuffix, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

func TestAssetHandlerRequiresNoAuthorization(t *testing.T) {
	handler, manifest, _ := newAssetHandler(t)
	request := httptest.NewRequest(http.MethodGet, assetPathPrefix+manifest.SHA256+assetPathSuffix, nil)
	if request.Header.Get("Authorization") != "" {
		t.Fatal("test request unexpectedly carries credentials")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}
