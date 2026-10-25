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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const (
	defaultBindAddress      = ":18080"
	defaultCertDir          = "/etc/coraza-tls"
	defaultCertName         = "tls.crt"
	defaultKeyName          = "tls.key"
	defaultCAPath           = "/etc/coraza-tls/ca.crt"
	certificateCheckPeriod  = 60 * time.Second
	handshakeTimeout        = 10 * time.Second
	connectionTimeout       = 10 * time.Second
	assetReadHeaderTimeout  = 10 * time.Second
	assetReadTimeout        = 15 * time.Second
	assetWriteTimeout       = 60 * time.Second
	assetIdleTimeout        = 2 * time.Minute
	assetMaxHeaderBytes     = 16 << 10
	shutdownTimeout         = 5 * time.Second
	connectionQueueCapacity = 64
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
	grpc    *grpc.Server
	http    *http.Server
	certs   *certificateReloader
	caPEM   []byte
	logger  logr.Logger

	mu       sync.Mutex
	listener net.Listener
	bound    bool
	started  chan struct{}
	ready    bool
}

// NewListener loads the serving certificate and CA, prepares the gRPC and HTTP
// servers, and returns a listener that binds when started.
func NewListener(config ListenerConfig, server *Server, handler http.Handler, limits Limits, logger logr.Logger) (*Listener, error) {
	config = config.withDefaults()
	limits = limits.withDefaults()

	certs, err := newCertificateReloader(config.CertDir, config.CertName, config.KeyName, logger)
	if err != nil {
		return nil, err
	}

	caPEM, err := os.ReadFile(config.CAPath)
	if err != nil {
		// Readiness reports the missing CA; the server stays up so the rest of
		// the operator keeps reconciling.
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
		grpc.ConnectionTimeout(connectionTimeout),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	server.Register(grpcServer)

	return &Listener{
		config:  config,
		handler: handler,
		grpc:    grpcServer,
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
	base, err := net.Listen("tcp", l.config.BindAddress)
	if err != nil {
		return fmt.Errorf("bind xDS listener on %s: %w", l.config.BindAddress, err)
	}
	tlsListener := tls.NewListener(base, l.tlsConfig())
	h2 := newConnListener(tlsListener.Addr())
	http1 := newConnListener(tlsListener.Addr())

	l.mu.Lock()
	l.listener = tlsListener
	l.bound = true
	l.ready = true
	close(l.started)
	l.mu.Unlock()

	serveErrors := make(chan error, 3)
	go func() { serveErrors <- l.grpc.Serve(h2) }()
	go func() { serveErrors <- l.http.Serve(http1) }()
	go func() { serveErrors <- l.accept(ctx, tlsListener, h2, http1) }()

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

func (l *Listener) accept(ctx context.Context, listener net.Listener, h2, http1 *connListener) error {
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go l.dispatch(ctx, connection, h2, http1)
	}
}

func (l *Listener) dispatch(ctx context.Context, connection net.Conn, h2, http1 *connListener) {
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
		if !h2.push(tlsConnection) {
			_ = tlsConnection.Close()
		}
	default:
		// http/1.1, including clients that offer no ALPN at all. gRPC clients
		// always offer h2, so only h2 is dispatched to the gRPC server.
		if !http1.push(tlsConnection) {
			_ = tlsConnection.Close()
		}
	}
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
	l.mu.Unlock()

	if listener != nil {
		_ = listener.Close()
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
// Certificate reloader
// -----------------------------------------------------------------------------

type certificateReloader struct {
	certPath string
	keyPath  string
	logger   logr.Logger
	now      func() time.Time

	mu          sync.Mutex
	certificate *tls.Certificate
	certMod     time.Time
	keyMod      time.Time
	certSize    int64
	keySize     int64
	lastCheck   time.Time
}

func newCertificateReloader(dir, certName, keyName string, logger logr.Logger) (*certificateReloader, error) {
	reloader := &certificateReloader{
		certPath: filepath.Join(dir, certName),
		keyPath:  filepath.Join(dir, keyName),
		logger:   logger,
		now:      time.Now,
	}
	if err := reloader.reload(); err != nil {
		// Readiness stays false until a certificate is loaded; the rest of the
		// operator continues to run.
		logger.Error(err, "Failed to load the xDS serving certificate")
	}
	return reloader, nil
}

// GetCertificate returns the current certificate, re-reading the files at most
// once per minute when their stat information changes. A failed reload keeps
// the previously loaded certificate.
func (r *certificateReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.now().Sub(r.lastCheck) >= certificateCheckPeriod {
		r.lastCheck = r.now()
		if err := r.reloadLocked(); err != nil {
			r.logger.Error(err, "Failed to reload xDS serving certificate; keeping the previous certificate")
		}
	}
	return r.certificate, nil
}

// Ready reports whether a certificate is loaded. It retries a failed initial
// load so readiness recovers when a mounted Secret appears after startup.
func (r *certificateReloader) Ready() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.certificate == nil {
		if err := r.reloadLocked(); err != nil {
			return fmt.Errorf("xDS serving certificate is not loaded: %w", err)
		}
	}
	return nil
}

func (r *certificateReloader) reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloadLocked()
}

func (r *certificateReloader) reloadLocked() error {
	certInfo, err := os.Stat(r.certPath)
	if err != nil {
		return fmt.Errorf("stat xDS certificate: %w", err)
	}
	keyInfo, err := os.Stat(r.keyPath)
	if err != nil {
		return fmt.Errorf("stat xDS key: %w", err)
	}
	if r.certificate != nil &&
		certInfo.ModTime().Equal(r.certMod) && certInfo.Size() == r.certSize &&
		keyInfo.ModTime().Equal(r.keyMod) && keyInfo.Size() == r.keySize {
		return nil
	}

	certificate, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return fmt.Errorf("load xDS serving certificate: %w", err)
	}
	r.certificate = &certificate
	r.certMod = certInfo.ModTime()
	r.certSize = certInfo.Size()
	r.keyMod = keyInfo.ModTime()
	r.keySize = keyInfo.Size()
	return nil
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

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.conns:
		return connection, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

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
