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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

type listenerFixture struct {
	listener *Listener
	server   *Server
	cancel   context.CancelFunc
	pool     *x509.CertPool
	manifest *assets.Manifest
}

func newListenerFixture(t *testing.T) *listenerFixture {
	return newListenerFixtureWithLimits(t, DefaultLimits())
}

func newListenerFixtureWithLimits(t *testing.T, limits Limits) *listenerFixture {
	return newListenerFixtureWithConfig(t, ListenerConfig{
		BindAddress: "127.0.0.1:0",
		CertDir:     "testdata",
		CertName:    "tls.crt",
		KeyName:     "tls.key",
		CAPath:      "testdata/ca.crt",
	}, limits)
}

func newListenerFixtureWithConfig(t *testing.T, config ListenerConfig, limits Limits) *listenerFixture {
	t.Helper()

	manifest, data, err := assets.ResolveDefault()
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	store := NewSnapshotStore()
	authz := NewAuthzIndex(nil, nil, time.Hour, nil)
	auth := NewAuthenticator(nil, "", time.Minute, time.Minute, time.Second, 0)
	streams := NewStreamRegistry(auth, authz, limits, metrics)
	server := NewServer(store, auth, authz, streams, limits, metrics)

	listener, err := NewListener(config, server, NewAssetHandler(manifest, data, metrics), limits, logr.Discard())
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

	return &listenerFixture{listener: listener, server: server, cancel: cancel, pool: pool, manifest: manifest}
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
		if err := stream.Send(&discoveryv3.DiscoveryRequest{ResourceNames: []string{ResourceName("shop", "checkout", "uid")}}); err != nil {
			t.Fatal(err)
		}
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

func TestListenerCapsConnections(t *testing.T) {
	// The connection cap is twice the stream limit, so one stream of headroom
	// plus one keeps asset fetches reachable while streams saturate the cap.
	limits := DefaultLimits()
	limits.MaxStreams = 1
	fixture := newListenerFixtureWithLimits(t, limits)
	address := fixture.listener.Addr().String()

	dial := func(timeout time.Duration) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: timeout}
		return tls.DialWithDialer(dialer, "tcp", address, &tls.Config{
			RootCAs:    fixture.pool,
			ServerName: "localhost",
			NextProtos: []string{"http/1.1"},
			MinVersion: tls.VersionTLS13,
		})
	}

	first, err := dial(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := dial(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	// The third connection queues behind the cap: the handshake never starts.
	if conn, err := dial(250 * time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("connection above the cap must not complete")
	}

	// Freeing a slot lets the next connection through again.
	_ = first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := dial(time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection below the cap must be accepted after a slot frees: %v", err)
		}
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
	return NewAssetHandler(manifest, data, metrics), manifest, data
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
