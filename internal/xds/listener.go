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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/net/http2"
	"golang.org/x/net/netutil"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

const (
	defaultBindAddress      = ":18080"
	defaultCertDir          = "/etc/coraza-tls"
	defaultCertName         = "tls.crt"
	defaultKeyName          = "tls.key"
	defaultCAPath           = "/etc/coraza-tls/ca.crt"
	handshakeTimeout        = 10 * time.Second
	assetReadHeaderTimeout  = 10 * time.Second
	assetReadTimeout        = 15 * time.Second
	assetWriteTimeout       = 60 * time.Second
	assetIdleTimeout        = 2 * time.Minute
	assetMaxHeaderBytes     = 16 << 10
	shutdownTimeout         = 5 * time.Second
	connectionQueueCapacity = 64
	// certificateRetryInterval is how often the certificate watcher retries
	// creation while the certificate files are missing.
	certificateRetryInterval = 5 * time.Second

	// h2MaxConcurrentStreams bounds HTTP/2 streams per connection: ECDS uses
	// one long-lived stream per proxy and asset fetches are short.
	h2MaxConcurrentStreams = 64
	// h2MaxReadFrameSize bounds one HTTP/2 frame from the peer; 16 KiB is the
	// protocol default, kept explicit as a DoS bound.
	h2MaxReadFrameSize = 1 << 14
)

// ListenerConfig configures the shared TLS listener.
type ListenerConfig struct {
	BindAddress string
	CertDir     string
	CertName    string
	KeyName     string
	CAPath      string
}

// Listener binds one TLS listener and dispatches ALPN h2 connections to the
// gRPC server and http/1.1 connections to the asset handler. The metrics TLS
// configuration is untouched.
type Listener struct {
	config  ListenerConfig
	handler http.Handler
	h2      *http2.Server
	grpc    *grpc.Server
	http    *http.Server
	certs   *certWatcher
	caPEM   []byte
	logger  logr.Logger
	limits  Limits

	mu          sync.Mutex
	listener    net.Listener
	connections map[net.Conn]struct{}
	bound       bool
	started     chan struct{}
	ready       bool
}

// NewListener loads the serving certificate and CA, prepares the gRPC and HTTP
// servers, and returns a listener that binds when started.
func NewListener(config ListenerConfig, server *Server, handler http.Handler, limits Limits, logger logr.Logger) (*Listener, error) {
	config = config.withDefaults()
	limits = limits.WithDefaults()

	certs := newCertWatcher(config.CertDir, config.CertName, config.KeyName, logger)

	caPEM, err := os.ReadFile(config.CAPath)
	if err != nil {
		// The CA is not hot-reloaded: it is read here and retried by Ready
		// until it appears (so a late-mounted Secret recovers), but rotating
		// it requires a restart. Readiness reports the missing CA; the server
		// stays up so the rest of the operator keeps reconciling.
		logger.Error(err, "Failed to read the xDS CA bundle", "path", config.CAPath)
		caPEM = nil
	} else {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			logger.Error(errors.New("no certificate found"), "Failed to parse the xDS CA bundle", "path", config.CAPath)
			caPEM = nil
		}
	}

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(limits.MaxMessageBytes),
		grpc.MaxSendMsgSize(limits.MaxMessageBytes),
		grpc.ChainStreamInterceptor(server.StreamInterceptor()),
	)
	server.Register(grpcServer)

	return &Listener{
		config:  config,
		handler: handler,
		h2: &http2.Server{
			IdleTimeout:          assetIdleTimeout,
			MaxConcurrentStreams: h2MaxConcurrentStreams,
			MaxReadFrameSize:     h2MaxReadFrameSize,
		},
		grpc: grpcServer,
		http: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: assetReadHeaderTimeout,
			ReadTimeout:       assetReadTimeout,
			WriteTimeout:      assetWriteTimeout,
			IdleTimeout:       assetIdleTimeout,
			MaxHeaderBytes:    assetMaxHeaderBytes,
		},
		certs:   certs,
		caPEM:   caPEM,
		logger:  logger,
		limits:  limits,
		started: make(chan struct{}),
	}, nil
}

func (c ListenerConfig) withDefaults() ListenerConfig {
	if c.BindAddress == "" {
		c.BindAddress = defaultBindAddress
	}
	if c.CertDir == "" {
		c.CertDir = defaultCertDir
	}
	if c.CertName == "" {
		c.CertName = defaultCertName
	}
	if c.KeyName == "" {
		c.KeyName = defaultKeyName
	}
	if c.CAPath == "" {
		c.CAPath = defaultCAPath
	}
	return c
}

// Start binds the listener and serves until ctx ends. It implements
// manager.Runnable.
func (l *Listener) Start(ctx context.Context) error {
	var listenConfig net.ListenConfig
	base, err := listenConfig.Listen(ctx, "tcp", l.config.BindAddress)
	if err != nil {
		return fmt.Errorf("bind xDS listener on %s: %w", l.config.BindAddress, err)
	}
	// Cap accepted connections above the stream limit: every ECDS stream needs
	// a connection, and the headroom keeps short-lived asset fetches from
	// being starved by long-lived stream connections.
	base = netutil.LimitListener(base, 2*l.limits.MaxStreams)
	tlsListener := tls.NewListener(base, l.tlsConfig())
	http1 := newConnListener(tlsListener.Addr())

	l.mu.Lock()
	l.listener = tlsListener
	l.bound = true
	l.ready = true
	close(l.started)
	l.mu.Unlock()

	serveErrors := make(chan error, 2)
	go func() { serveErrors <- l.http.Serve(http1) }()
	go func() { serveErrors <- l.accept(ctx, tlsListener, http1) }()
	// Certificate watching is best-effort: an fsnotify failure must not take
	// down the serving plane. Retry it until the process context is canceled.
	go func() {
		for {
			err := l.certs.Start(ctx)
			if err == nil || ctx.Err() != nil {
				return
			}
			l.logger.Error(err, "xDS certificate watcher stopped; retrying", "backoff", certificateRetryInterval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(certificateRetryInterval):
			}
		}
	}()

	select {
	case <-ctx.Done():
		l.shutdown()
		return nil
	case err := <-serveErrors:
		l.shutdown()
		if errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

// NeedLeaderElection keeps the serving plane running on every replica.
func (l *Listener) NeedLeaderElection() bool {
	return false
}

// Started returns a channel closed once the listener is bound.
func (l *Listener) Started() <-chan struct{} {
	return l.started
}

// Addr returns the bound address, or nil before Start.
func (l *Listener) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.listener == nil {
		return nil
	}
	return l.listener.Addr()
}

// Ready reports whether the listener is bound and serving material is loaded.
// It retries a missing certificate or CA so readiness recovers when a mounted
// Secret appears after startup.
func (l *Listener) Ready() error {
	l.mu.Lock()
	bound := l.bound && l.ready
	if bound && len(l.caPEM) == 0 {
		if caPEM, err := os.ReadFile(l.config.CAPath); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(caPEM) {
				l.caPEM = caPEM
			}
		}
	}
	caLoaded := len(l.caPEM) > 0
	l.mu.Unlock()

	if !bound {
		return errors.New("xDS listener is not bound")
	}
	if err := l.certs.Ready(); err != nil {
		return err
	}
	if !caLoaded {
		return errors.New("xDS CA bundle is empty")
	}
	return nil
}

// Close stops the listener. It is safe to call more than once.
func (l *Listener) Close() {
	l.shutdown()
}

func (l *Listener) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: l.certs.GetCertificate,
	}
}

func (l *Listener) accept(ctx context.Context, listener net.Listener, http1 *connListener) error {
	var delay time.Duration
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Mirror net/http's Serve: a temporary error such as EMFILE or
			// ECONNABORTED backs off and retries instead of tearing the
			// listener down.
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
				}
				if delay > time.Second {
					delay = time.Second
				}
				l.logger.Error(err, "xDS listener accept error; retrying", "backoff", delay)
				time.Sleep(delay)
				continue
			}
			return err
		}
		go l.dispatch(ctx, connection, http1)
	}
}

func (l *Listener) dispatch(ctx context.Context, connection net.Conn, http1 *connListener) {
	tlsConnection, ok := connection.(*tls.Conn)
	if !ok {
		_ = connection.Close()
		return
	}

	handshakeCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	err := tlsConnection.HandshakeContext(handshakeCtx)
	cancel()
	if err != nil {
		_ = tlsConnection.Close()
		return
	}

	switch tlsConnection.ConnectionState().NegotiatedProtocol {
	case "h2":
		// Serve gRPC and the asset route on the same HTTP/2 connection: Envoy
		// speaks gRPC, while tools such as curl default to HTTP/2 for both.
		l.track(tlsConnection)
		l.h2.ServeConn(tlsConnection, &http2.ServeConnOpts{
			Handler: http.HandlerFunc(l.serveH2),
			// BaseConfig carries the header limit to HTTP/2. The HTTP/1
			// write and read timeouts are deliberately dropped: x/net/http2
			// applies them as per-stream deadlines, which would reset
			// long-lived ECDS streams that sit idle between pushes.
			BaseConfig: &http.Server{
				MaxHeaderBytes:    assetMaxHeaderBytes,
				ReadHeaderTimeout: assetReadHeaderTimeout,
			},
		})
		l.untrack(tlsConnection)
	default:
		// http/1.1, including clients that offer no ALPN at all. gRPC clients
		// always offer h2, so only h2 is dispatched to the gRPC server.
		if !http1.push(tlsConnection) {
			_ = tlsConnection.Close()
		}
	}
}

// serveH2 routes a gRPC request to the xDS server and everything else to the
// asset handler, so /wasm/<digest>.wasm is reachable over HTTP/2 too.
func (l *Listener) serveH2(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		l.grpc.ServeHTTP(w, r)
		return
	}
	l.handler.ServeHTTP(w, r)
}

func (l *Listener) track(connection net.Conn) {
	l.mu.Lock()
	if l.connections == nil {
		l.connections = make(map[net.Conn]struct{})
	}
	l.connections[connection] = struct{}{}
	l.mu.Unlock()
}

func (l *Listener) untrack(connection net.Conn) {
	l.mu.Lock()
	delete(l.connections, connection)
	l.mu.Unlock()
}

func (l *Listener) shutdown() {
	l.mu.Lock()
	if !l.bound {
		l.mu.Unlock()
		return
	}
	l.bound = false
	l.ready = false
	listener := l.listener
	connections := l.connections
	l.connections = nil
	l.mu.Unlock()

	if listener != nil {
		_ = listener.Close()
	}
	for connection := range connections {
		_ = connection.Close()
	}

	stopped := make(chan struct{})
	go func() {
		l.grpc.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(shutdownTimeout):
		l.grpc.Stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = l.http.Shutdown(shutdownCtx)
}

// -----------------------------------------------------------------------------
// Certificate watcher
// -----------------------------------------------------------------------------

// certWatcher serves the xDS certificate via controller-runtime's
// certwatcher (fsnotify with an interval fallback). certwatcher.New rejects
// missing files, so creation is deferred until they exist: NewListener must
// succeed and readiness must recover when the mounted Secret appears after
// startup. The CA bundle is not covered; see NewListener.
type certWatcher struct {
	certPath string
	keyPath  string

	mu      sync.Mutex
	watcher *certwatcher.CertWatcher
}

func newCertWatcher(dir, certName, keyName string, logger logr.Logger) *certWatcher {
	w := &certWatcher{
		certPath: filepath.Join(dir, certName),
		keyPath:  filepath.Join(dir, keyName),
	}
	if _, err := w.getOrCreate(); err != nil {
		// Readiness stays false until a certificate is loaded; the rest of the
		// operator continues to run.
		logger.Error(err, "Failed to load the xDS serving certificate")
	}
	return w
}

// getOrCreate returns the watcher, creating it once the files exist. The
// initial certwatcher.New call also loads the certificate.
func (r *certWatcher) getOrCreate() (*certwatcher.CertWatcher, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.watcher == nil {
		watcher, err := certwatcher.New(r.certPath, r.keyPath)
		if err != nil {
			return nil, err
		}
		r.watcher = watcher
	}
	return r.watcher, nil
}

// GetCertificate returns the current certificate, or nil before one is
// loaded; TLS handshakes fail until then, as before.
func (r *certWatcher) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	watcher := r.watcher
	r.mu.Unlock()
	if watcher == nil {
		return nil, nil
	}
	return watcher.GetCertificate(nil)
}

// Ready reports whether a certificate is loaded, creating the watcher when
// the files have appeared so readiness recovers without a restart.
func (r *certWatcher) Ready() error {
	if _, err := r.getOrCreate(); err != nil {
		return fmt.Errorf("xDS serving certificate is not loaded: %w", err)
	}
	return nil
}

// Start runs the watcher until ctx ends. While the files are missing it
// retries creation instead of failing, so the listener stays up.
func (r *certWatcher) Start(ctx context.Context) error {
	watcher, err := r.getOrCreate()
	for err != nil {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(certificateRetryInterval):
		}
		watcher, err = r.getOrCreate()
	}
	return watcher.Start(ctx)
}

// -----------------------------------------------------------------------------
// Channel-backed listener
// -----------------------------------------------------------------------------

type connListener struct {
	addr  net.Addr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newConnListener(addr net.Addr) *connListener {
	return &connListener{
		addr:  addr,
		conns: make(chan net.Conn, connectionQueueCapacity),
		done:  make(chan struct{}),
	}
}

// Accept returns the next HTTP/1.1 connection queued by dispatch.
func (l *connListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.conns:
		return connection, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close unblocks Accept and stops accepting connections.
func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

// Addr returns the address of the underlying TLS listener.
func (l *connListener) Addr() net.Addr {
	return l.addr
}

func (l *connListener) push(connection net.Conn) bool {
	select {
	case l.conns <- connection:
		return true
	case <-l.done:
		return false
	}
}

const (
	assetPathPrefix   = "/wasm/"
	assetPathSuffix   = ".wasm"
	wasmContentType   = "application/wasm"
	immutableCacheMax = "public, max-age=31536000, immutable"
)

var assetNamePattern = regexp.MustCompile(`^[0-9a-f]{64}\.wasm$`)

// AssetHandler serves the single bundled WASM module by digest. Only manifest
// entries are addressable; the handler never touches the filesystem, never
// redirects, and requires no bearer token.
type AssetHandler struct {
	manifest *assets.Manifest
	data     []byte
	metrics  *Metrics
}

// NewAssetHandler returns a handler for the verified bundled module.
func NewAssetHandler(manifest *assets.Manifest, data []byte, metrics *Metrics) *AssetHandler {
	return &AssetHandler{manifest: manifest, data: data, metrics: metrics}
}

// ServeHTTP implements http.Handler.
func (h *AssetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	code := http.StatusNotFound
	defer func() { h.metrics.AssetRequest(code) }()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		code = http.StatusMethodNotAllowed
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(code)
		return
	}

	name, ok := assetNameFromPath(r.URL.Path)
	if !ok || h.manifest == nil || name != h.manifest.SHA256+assetPathSuffix {
		w.WriteHeader(code)
		return
	}

	code = http.StatusOK
	w.Header().Set("Content-Type", wasmContentType)
	w.Header().Set("Cache-Control", immutableCacheMax)
	recorder := &assetStatusWriter{ResponseWriter: w, status: code}
	// ServeContent adds Range/If-Range support and Content-Length; the digest
	// path remains the only addressable name.
	http.ServeContent(recorder, r, name, time.Time{}, bytes.NewReader(h.data))
	code = recorder.status
}

type assetStatusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader records and forwards the response status.
func (w *assetStatusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func assetNameFromPath(path string) (string, bool) {
	if !strings.HasPrefix(path, assetPathPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(path, assetPathPrefix)
	if !assetNamePattern.MatchString(name) {
		return "", false
	}
	return name, true
}
