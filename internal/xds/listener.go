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
	"container/list"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

	// h2MaxConcurrentStreams bounds HTTP/2 streams per connection: Envoy
	// keeps one long-lived ECDS stream per pod and asset fetches are short.
	h2MaxConcurrentStreams = 8
	// h2MaxReadFrameSize bounds one HTTP/2 frame from the peer; 16 KiB is the
	// protocol default, kept explicit as a DoS bound.
	h2MaxReadFrameSize = 1 << 14

	// perSourceShare sizes the default per-source share of the pool of
	// connections without an admitted ECDS stream as a share (1/n) of
	// MaxStreams, which is the pool's size.
	perSourceShare = 16
	// h2IdleTimeout closes an HTTP/2 connection that carries no stream for
	// this long, including one that never starts a request.
	h2IdleTimeout = 15 * time.Second
	// h2UploadBuffer is the receive window per connection and per stream.
	h2UploadBuffer = 64 << 10
	// h2ReadIdleTimeout and h2PingTimeout close a connection whose peer stops
	// answering: after a minute without frames it is pinged.
	h2ReadIdleTimeout = time.Minute
	h2PingTimeout     = 15 * time.Second
	// writeChunk bounds how much of a response one armed write deadline
	// covers. gRPC hands a whole response message to one Write, so the
	// deadline bounds progress rather than message size.
	writeChunk = 256 << 10
	// minResponseWriteRate bounds a whole ECDS response's write time to the
	// write timeout plus its size at this rate, so a peer that reads just
	// fast enough for each chunk still cannot hold a response for long.
	minResponseWriteRate = 64 << 10

	// requestReadTimeout bounds how long one ECDS request message may take to
	// arrive once its first byte is read. Envoy writes each request at once.
	requestReadTimeout = 10 * time.Second
	// grpcMessageHeaderBytes is the gRPC length-prefixed message header: one
	// compression flag byte and a four-byte big-endian length.
	grpcMessageHeaderBytes = 5
	// grpcContentType is the gRPC media type.
	grpcContentType = "application/grpc"
	// messagesAhead bounds how many request messages a stream reads ahead of
	// its handler: Envoy has at most an acknowledgement and a subscription
	// change outstanding. Each can be up to MaxRequestBytes.
	messagesAhead = 2
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
	handler *AssetHandler
	server  *Server
	h2      *http2.Server
	grpc    *grpc.Server
	http    *http.Server
	certs   *certWatcher
	ca      *CASource
	expiry  *certificateExpiry
	logger  logr.Logger
	limits  Limits
	// requestTimeout bounds one request message once its first byte is read.
	// It defaults to requestReadTimeout and is a field so tests can shorten it.
	requestTimeout time.Duration
	// minWriteRate is minResponseWriteRate, a field so tests can raise it.
	minWriteRate int

	mu          sync.Mutex
	listener    net.Listener
	connections map[net.Conn]struct{}
	bound       bool
	started     chan struct{}
	ready       bool
}

// NewListener loads the serving certificate and CA, prepares the gRPC and HTTP
// servers, and returns a listener that binds when started.
func NewListener(config ListenerConfig, server *Server, handler *AssetHandler, limits Limits, logger logr.Logger) (*Listener, error) {
	config = config.withDefaults()
	limits = limits.WithDefaults()

	expiry := newCertificateExpiry(server.Metrics, logger)
	certs := newCertWatcher(config.CertDir, config.CertName, config.KeyName, expiry.observeServingCert, logger)

	// The CA is loaded on demand and cached by CASource. Ready retries a
	// missing or unparsable bundle so a late-mounted Secret recovers, and the
	// Engine reconciler shares the same source so new EnvoyFilters carry it.
	ca := NewCASource(config.CAPath, logger)
	ca.onLoad = expiry.observeCA
	if ca.PEM() == nil {
		logger.Error(errors.New("no certificate found"), "Failed to read the xDS CA bundle", "path", config.CAPath)
	}

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(limits.MaxRequestBytes),
		grpc.MaxSendMsgSize(limits.MaxMessageBytes),
		grpc.ChainStreamInterceptor(server.StreamInterceptor()),
	)
	server.Register(grpcServer)

	l := &Listener{
		config:  config,
		handler: handler,
		server:  server,
		h2: &http2.Server{
			IdleTimeout:          h2IdleTimeout,
			MaxConcurrentStreams: h2MaxConcurrentStreams,
			MaxReadFrameSize:     h2MaxReadFrameSize,
			// A peer that stops reading at the TCP level blocks the
			// connection's writer, where a stream's write deadline can only
			// queue a reset; close the connection once a write makes no
			// progress, which ends all of its streams.
			WriteByteTimeout: limits.WriteTimeout,
			ReadIdleTimeout:  h2ReadIdleTimeout,
			PingTimeout:      h2PingTimeout,
			// Bound what a peer may send before a handler reads it, such as a
			// request body while its token is reviewed. Envoy's largest
			// request, the first on a stream, is about 47 KB.
			MaxUploadBufferPerConnection: h2UploadBuffer,
			MaxUploadBufferPerStream:     h2UploadBuffer,
		},
		grpc: grpcServer,
		http: &http.Server{
			ConnContext: func(ctx context.Context, c net.Conn) context.Context {
				return context.WithValue(ctx, connKey{}, c.(*tls.Conn).NetConn())
			},
			ReadHeaderTimeout: assetReadHeaderTimeout,
			ReadTimeout:       assetReadTimeout,
			WriteTimeout:      assetWriteTimeout,
			IdleTimeout:       assetIdleTimeout,
			MaxHeaderBytes:    assetMaxHeaderBytes,
		},
		certs:          certs,
		ca:             ca,
		expiry:         expiry,
		logger:         logger,
		limits:         limits,
		requestTimeout: requestReadTimeout,
		minWriteRate:   minResponseWriteRate,
		started:        make(chan struct{}),
	}
	l.http.Handler = http.HandlerFunc(l.serveHTTP1)
	return l, nil
}

// connKey carries an HTTP/1.1 request's trackedConn in its context.
type connKey struct{}

// serveHTTP1 serves the asset route on an HTTP/1.1 connection.
func (l *Listener) serveHTTP1(w http.ResponseWriter, r *http.Request) {
	l.serveAsset(r.Context().Value(connKey{}).(*trackedConn), w, r)
}

// serveAsset serves the asset route; a module download is protected from
// being closed for room, see trackedConn.rankLocked.
func (l *Listener) serveAsset(conn *trackedConn, w http.ResponseWriter, r *http.Request) {
	defer conn.request(r.Method == http.MethodGet && l.handler.serves(r.URL.Path))()
	l.handler.ServeHTTP(w, r)
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
	base = &connectionPool{
		Listener:      base,
		maxUnadmitted: l.limits.MaxStreams,
		maxPerSource:  l.limits.MaxConnectionsPerSource,
		metrics:       l.server.Metrics,
		perSource:     map[string]int{},
	}
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
	go l.expiry.run(ctx)
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
	l.mu.Unlock()

	if !bound {
		return errors.New("xDS listener is not bound")
	}
	if err := l.certs.Ready(); err != nil {
		return err
	}
	return l.ca.Ready()
}

// CASource exposes the CA bundle the listener loaded so the Engine reconciler
// reads the same value.
func (l *Listener) CASource() *CASource {
	return l.ca
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
		conn := tlsConnection.NetConn().(*trackedConn)
		l.track(tlsConnection)
		l.h2.ServeConn(tlsConnection, &http2.ServeConnOpts{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { l.serveH2(conn, w, r) }),
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
// asset handler, so /wasm/<digest>.wasm is reachable over HTTP/2 too. A gRPC
// request is authenticated from its headers first; a refused request is
// answered with a gRPC status and its body is never read. While the request
// holds an admitted stream, conn is out of the pools of connections without
// one; before that, conn records the request for the pools' eviction order.
func (l *Listener) serveH2(conn *trackedConn, w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, grpcContentType) {
		l.serveAsset(conn, w, r)
		return
	}
	defer conn.request(false)()
	// Refuse a request grpc-go would refuse for its method, media type,
	// timeout or binary metadata before any TokenReview.
	if r.Method != http.MethodPost || !validGRPCContentType(contentType) || r.URL.Path != ecdsStreamMethod ||
		!validGRPCMetadata(r.Header) {
		l.server.Metrics.Refused(RefusalMethod)
		writeGRPCStatus(w, status.Error(codes.Unimplemented, "unknown method"))
		return
	}
	admission, err := l.server.admit(r, conn.review)
	if err != nil {
		writeGRPCStatus(w, err)
		return
	}
	defer l.server.release(admission)
	conn.admit()
	defer conn.unadmit()

	// Build the controller on the original writer; the wrappers below do not
	// unwrap to it.
	controller := http.NewResponseController(w)
	admission.body = &requestBody{
		ReadCloser: r.Body,
		controller: controller,
		timeout:    l.requestTimeout,
		maxBytes:   l.limits.MaxRequestBytes,
		metrics:    l.server.Metrics,
		ahead:      make(chan struct{}, messagesAhead),
		done:       r.Context().Done(),
		closed:     make(chan struct{}),
	}
	r.Body = admission.body
	admission.writer = &deadlineWriter{
		ResponseWriter: w,
		controller:     controller,
		timeout:        l.limits.WriteTimeout,
		minRate:        l.minWriteRate,
		metrics:        l.server.Metrics,
	}
	l.grpc.ServeHTTP(admission.writer, r.WithContext(context.WithValue(r.Context(), admissionKey{}, admission)))
}

// validGRPCContentType reports whether grpc-go accepts contentType: the gRPC
// media type alone or followed by "+" and a subtype or ";" and parameters.
func validGRPCContentType(contentType string) bool {
	rest, ok := strings.CutPrefix(contentType, grpcContentType)
	return ok && (rest == "" || rest[0] == '+' || rest[0] == ';')
}

// grpcTimeoutPattern is grpc-go's timeout syntax: up to eight digits and a
// unit.
var grpcTimeoutPattern = regexp.MustCompile(`^[0-9]{1,8}[HMSmun]$`)

// validGRPCMetadata reports whether grpc-go accepts the request's timeout and
// binary metadata, which it decodes as standard base64, padded when the value
// length is a multiple of four and unpadded otherwise.
func validGRPCMetadata(header http.Header) bool {
	if timeout := header.Get("Grpc-Timeout"); timeout != "" && !grpcTimeoutPattern.MatchString(timeout) {
		return false
	}
	for name, values := range header {
		if len(name) < 4 || !strings.EqualFold(name[len(name)-4:], "-bin") {
			continue
		}
		for _, value := range values {
			encoding := base64.RawStdEncoding
			if len(value)%4 == 0 {
				encoding = base64.StdEncoding
			}
			if _, err := encoding.DecodeString(value); err != nil {
				return false
			}
		}
	}
	return true
}

// writeGRPCStatus answers a gRPC request with a trailers-only status. Status
// messages are fixed ASCII strings, so they need no percent-encoding.
func writeGRPCStatus(w http.ResponseWriter, err error) {
	st := status.Convert(err)
	header := w.Header()
	header.Set("Content-Type", grpcContentType)
	header.Set("Grpc-Status", strconv.Itoa(int(st.Code())))
	header.Set("Grpc-Message", st.Message())
	w.WriteHeader(http.StatusOK)
}

// expired is a write deadline in the past: setting it resets the stream.
var expired = time.Unix(1, 0)

// deadlineWriter arms a write deadline for every writeChunk bytes written and
// for Flush, and restores the resting deadline after Flush, so a peer that
// stops reading is reset within timeout while an idle stream between
// responses is kept. Under grpc.Server.ServeHTTP, SendMsg returns once a write
// is handed over, so the deadline must wrap the ResponseWriter, not SendMsg.
type deadlineWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
	// minRate, when set, bounds a whole message's write time to timeout plus
	// its size at minRate bytes per second.
	minRate int
	// rest is the deadline between writes; zero means none.
	rest    time.Time
	metrics *Metrics

	mu      sync.Mutex
	writing bool      // a message is written and not yet flushed
	aborted bool      // the stream ended; message writes fail
	message time.Time // bound on the message being written; zero for none
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	// A gRPC message header fits the server's write buffer; it never blocks.
	if len(p) <= grpcMessageHeaderBytes {
		return w.ResponseWriter.Write(p)
	}
	w.mu.Lock()
	w.writing = true
	if w.minRate > 0 {
		w.message = time.Now().Add(w.timeout + time.Duration(len(p))*time.Second/time.Duration(w.minRate))
	}
	w.mu.Unlock()
	written := 0
	for len(p) > 0 {
		deadline := w.arm()
		n, err := w.ResponseWriter.Write(p[:min(len(p), writeChunk)])
		written += n
		if err != nil {
			w.countTimeout(deadline)
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// Flush implements http.Flusher, which grpc.Server.ServeHTTP requires. Every
// gRPC write ends with it, so it also clears the deadline the writes armed. A
// message stays in flight until its flush returns: a small one fits the
// server's write buffer and waits for flow-control credit only here.
func (w *deadlineWriter) Flush() {
	deadline := w.arm()
	err := w.controller.Flush()
	w.mu.Lock()
	w.writing = false
	w.message = time.Time{}
	if !w.aborted {
		_ = w.controller.SetWriteDeadline(w.rest)
	}
	w.mu.Unlock()
	if err != nil {
		w.countTimeout(deadline)
	}
}

// arm sets the deadline for the next write: timeout from now, capped by the
// message bound and the resting deadline, or expired for a message once
// aborted.
func (w *deadlineWriter) arm() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	deadline := time.Now().Add(w.timeout)
	for _, bound := range []time.Time{w.message, w.rest} {
		if !bound.IsZero() && bound.Before(deadline) {
			deadline = bound
		}
	}
	if w.aborted && w.writing {
		deadline = expired
	}
	_ = w.controller.SetWriteDeadline(deadline)
	return deadline
}

// abort resets the stream if a message is being written or flushed and fails
// the messages after it, so a stream whose handler ended does not keep a
// response in flight. A flush with no message pending, such as the stream's
// status, is not reset.
func (w *deadlineWriter) abort() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.aborted = true
	if w.writing {
		_ = w.controller.SetWriteDeadline(expired)
	}
}

// countTimeout counts a failed write that ended at its deadline, but not one
// that abort ended.
func (w *deadlineWriter) countTimeout(deadline time.Time) {
	if !deadline.Equal(expired) && !time.Now().Before(deadline) {
		w.metrics.Refused(RefusalWriteTimeout)
	}
}

// requestBody hands the gRPC handler whole request messages. grpc-go's
// ServeHTTP transport reads the body on its own goroutine into one pooled
// 16 KiB buffer per Read and keeps every buffer until a message completes, so
// a peer sending one-byte frames would hold about 16 KiB of heap per message
// byte. Filling each Read up to the end of the current message bounds that to
// the message size; a message above maxBytes is refused, and one that does not
// arrive within timeout of its first byte is cut by a read deadline.
// The time before a message starts is not bounded, so idle streams are kept.
// grpc-go also queues every message without waiting for the handler, so the
// body reads at most messagesAhead messages ahead of it, holding the rest back
// in the peer's flow-control window.
type requestBody struct {
	io.ReadCloser
	controller *http.ResponseController
	timeout    time.Duration
	maxBytes   int
	metrics    *Metrics
	// ahead holds one token per message read and not yet received by the
	// handler; received takes one back.
	ahead  chan struct{}
	done   <-chan struct{} // the request ended
	closed chan struct{}   // the body was closed
	once   sync.Once

	header    [grpcMessageHeaderBytes]byte
	pending   []byte // header bytes not yet returned
	remaining int    // message bytes not yet read
}

var errRequestTooLarge = errors.New("request message exceeds the request size limit")

func (b *requestBody) Read(p []byte) (int, error) {
	if len(b.pending) == 0 && b.remaining == 0 {
		if err := b.nextMessage(); err != nil {
			return 0, err
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	if want := min(len(p)-n, b.remaining); want > 0 {
		read, err := io.ReadFull(b.ReadCloser, p[n:n+want])
		n += read
		b.remaining -= read
		if err != nil {
			return n, b.failed(err)
		}
	}
	if len(b.pending) == 0 && b.remaining == 0 {
		_ = b.controller.SetReadDeadline(time.Time{})
	}
	return n, nil
}

// nextMessage waits until the handler has room for another message and reads
// its header. Neither the wait nor waiting for the first byte is bounded; the
// rest of the message is bounded from then on.
func (b *requestBody) nextMessage() error {
	select {
	case b.ahead <- struct{}{}:
	case <-b.closed:
		return http.ErrBodyReadAfterClose
	case <-b.done:
		return context.Canceled
	}
	if _, err := io.ReadFull(b.ReadCloser, b.header[:1]); err != nil {
		return err
	}
	_ = b.controller.SetReadDeadline(time.Now().Add(b.timeout))
	if _, err := io.ReadFull(b.ReadCloser, b.header[1:]); err != nil {
		return b.failed(err)
	}
	length := binary.BigEndian.Uint32(b.header[1:])
	if int64(length) > int64(b.maxBytes) {
		b.metrics.Refused(RefusalRequestSize)
		return errRequestTooLarge
	}
	b.pending = b.header[:]
	b.remaining = int(length)
	return nil
}

// received records that the handler received a message, so one more can be
// read ahead of it.
func (b *requestBody) received() {
	select {
	case <-b.ahead:
	default:
	}
}

// Close wakes a read waiting for the handler; grpc-go closes the body and
// waits for its reader when the stream ends.
func (b *requestBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return b.ReadCloser.Close()
}

func (b *requestBody) failed(err error) error {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		b.metrics.Refused(RefusalSlowRequest)
	}
	return err
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
	// onLoad runs after every certificate load, including reloads, with a
	// function that returns the current certificate.
	onLoad func(current func(*tls.ClientHelloInfo) (*tls.Certificate, error))

	mu      sync.Mutex
	watcher *certwatcher.CertWatcher
}

func newCertWatcher(dir, certName, keyName string, onLoad func(func(*tls.ClientHelloInfo) (*tls.Certificate, error)), logger logr.Logger) *certWatcher {
	w := &certWatcher{
		certPath: filepath.Join(dir, certName),
		keyPath:  filepath.Join(dir, keyName),
		onLoad:   onLoad,
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
		// The watcher calls back under its own lock on registration, so the
		// current certificate is read from another goroutine.
		watcher.RegisterCallback(func(tls.Certificate) { go r.onLoad(r.GetCertificate) })
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
// Connection pools
// -----------------------------------------------------------------------------

// connectionPool holds every connection that carries no admitted ECDS stream
// (one in its handshake, a module download, or one before its first stream)
// in a global pool, at most maxPerSource of them per source address. A
// newcomer over the pool displaces a connection of the source holding the
// most, so a source flooding connections displaces its own, never one of a
// source holding fewer; a newcomer over its source's share displaces one of
// that source. This is fair while a flood comes from fewer source addresses
// than the pool holds connections. Within those sources the oldest connection without a request
// goes first, then the oldest with one, and a protected one, which serves a
// module download or a request whose token is under review, only when nothing
// else is left. Connections that carry admitted streams are bounded by the
// stream limits and never closed for room. Under pressure a module download
// can still be closed this way, and Envoy retries it.
type connectionPool struct {
	net.Listener
	maxUnadmitted int
	maxPerSource  int
	metrics       *Metrics

	mu         sync.Mutex
	unadmitted list.List      // *trackedConn, oldest first
	perSource  map[string]int // unadmitted connections per source
}

// Accept returns the next connection that is not itself closed for room.
func (p *connectionPool) Accept() (net.Conn, error) {
	for {
		connection, err := p.Listener.Accept()
		if err != nil {
			return nil, err
		}
		c := &trackedConn{Conn: connection, pool: p, source: connectionSource(connection.RemoteAddr())}
		evicted := p.add(c)
		p.closeEvicted(evicted)
		if !slices.Contains(evicted, c) {
			return c, nil
		}
	}
}

// add joins c to the pools and returns the connections closed for room,
// which can include c.
func (p *connectionPool) add(c *trackedConn) []*trackedConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.joinPoolsLocked(c)
}

// joinPoolsLocked adds c to the pools as their newest member and takes
// connections out for room, which can include c; closeEvicted closes them.
func (p *connectionPool) joinPoolsLocked(c *trackedConn) []*trackedConn {
	c.inPool = p.unadmitted.PushBack(c)
	p.perSource[c.source]++
	var evicted []*trackedConn
	for p.perSource[c.source] > p.maxPerSource {
		evicted = append(evicted, p.removeLocked(p.victimLocked(func(o *trackedConn) bool { return o.source == c.source })))
	}
	for p.unadmitted.Len() > p.maxUnadmitted {
		most := 0
		for _, n := range p.perSource {
			most = max(most, n)
		}
		evicted = append(evicted, p.removeLocked(p.victimLocked(func(o *trackedConn) bool { return p.perSource[o.source] == most })))
	}
	return evicted
}

// closeEvicted closes connections taken out for room. It runs without p.mu,
// which Close takes.
func (p *connectionPool) closeEvicted(evicted []*trackedConn) {
	for _, old := range evicted {
		p.metrics.Refused(RefusalConnectionEvicted)
		_ = old.Close()
	}
}

// victimLocked returns the connection in scope to close for room: the oldest
// of the lowest rank.
func (p *connectionPool) victimLocked(inScope func(*trackedConn) bool) *trackedConn {
	var victim *trackedConn
	rank := 0
	for e := p.unadmitted.Front(); e != nil; e = e.Next() {
		c := e.Value.(*trackedConn)
		if !inScope(c) {
			continue
		}
		if r := c.rankLocked(); victim == nil || r < rank {
			victim, rank = c, r
			if rank == 0 {
				break
			}
		}
	}
	return victim
}

func (p *connectionPool) removeLocked(c *trackedConn) *trackedConn {
	c.closed = true
	p.leavePoolsLocked(c)
	return c
}

func (p *connectionPool) leavePoolsLocked(c *trackedConn) {
	if c.inPool == nil {
		return
	}
	p.unadmitted.Remove(c.inPool)
	c.inPool = nil
	decrement(p.perSource, c.source)
}

// connectionSource keys the connection pools by source host, and an IPv6 host
// by its /64, which one host or pod network usually holds whole.
func connectionSource(addr net.Addr) string {
	host := sourceHost(addr)
	if ip, err := netip.ParseAddr(host); err == nil && !ip.Unmap().Is4() {
		prefix, _ := ip.WithZone("").Prefix(64)
		return prefix.String()
	}
	return host
}

// trackedConn is one connection in a connectionPool.
type trackedConn struct {
	net.Conn
	pool    *connectionPool
	source  string
	stalled atomic.Bool

	// Guarded by pool.mu.
	admitted  int
	closed    bool
	inPool    *list.Element // nil while admitted or closed
	requests  int           // requests in progress
	protected int           // module downloads being written and tokens under review
}

// rankLocked orders connections for closing: 0 in a handshake or without a
// request, 1 with a request in progress, 2 protected. A token review is
// protected while it runs, bounded by the review timeout and in number by the
// authentication slots; a GET of the published module while its response is
// written, bounded by the asset write timeout.
func (c *trackedConn) rankLocked() int {
	switch {
	case c.protected > 0:
		return 2
	case c.requests > 0:
		return 1
	}
	return 0
}

// request records a request in progress, protected when it downloads the
// module, and returns the function that ends it.
func (c *trackedConn) request(download bool) (end func()) {
	protected := 0
	if download {
		protected = 1
	}
	c.track(1, protected)
	return func() { c.track(-1, -protected) }
}

// review records a request whose token is under review, which is protected,
// and returns the function that ends it.
func (c *trackedConn) review() (end func()) {
	c.track(0, 1)
	return func() { c.track(0, -1) }
}

func (c *trackedConn) track(requests, protected int) {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	c.requests += requests
	c.protected += protected
}

// admit takes the connection out of the pools while it carries an admitted
// ECDS stream.
func (c *trackedConn) admit() {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	c.admitted++
	c.pool.leavePoolsLocked(c)
}

// unadmit returns the connection to the pools, as their newest member, when
// its last admitted stream ends, under the same caps as a newcomer.
func (c *trackedConn) unadmit() {
	var evicted []*trackedConn
	c.pool.mu.Lock()
	c.admitted--
	if c.admitted == 0 && !c.closed {
		evicted = c.pool.joinPoolsLocked(c)
	}
	c.pool.mu.Unlock()
	c.pool.closeEvicted(evicted)
}

// Write fails at once after a write has timed out. TLS gives its close alert
// a 5s write deadline, which would otherwise keep a connection whose peer
// stopped reading, and its streams' slots, that much longer.
func (c *trackedConn) Write(p []byte) (int, error) {
	if c.stalled.Load() {
		return 0, os.ErrDeadlineExceeded
	}
	n, err := c.Conn.Write(p)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		c.stalled.Store(true)
	}
	return n, err
}

// Close takes the connection out of the pools.
func (c *trackedConn) Close() error {
	c.pool.mu.Lock()
	c.pool.removeLocked(c)
	c.pool.mu.Unlock()
	return c.Conn.Close()
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
	manifest     *assets.Manifest
	data         []byte
	metrics      *Metrics
	writeTimeout time.Duration
}

// NewAssetHandler returns a handler for the verified bundled module. Each
// write to a client must complete within writeTimeout.
func NewAssetHandler(manifest *assets.Manifest, data []byte, metrics *Metrics, writeTimeout time.Duration) *AssetHandler {
	return &AssetHandler{manifest: manifest, data: data, metrics: metrics, writeTimeout: writeTimeout}
}

// serves reports whether path names the published module.
func (h *AssetHandler) serves(path string) bool {
	name, ok := assetNameFromPath(path)
	return ok && h.manifest != nil && name == h.manifest.SHA256+assetPathSuffix
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

	if !h.serves(r.URL.Path) {
		w.WriteHeader(code)
		return
	}
	name := h.manifest.SHA256 + assetPathSuffix

	code = http.StatusOK
	w.Header().Set("Content-Type", wasmContentType)
	w.Header().Set("Cache-Control", immutableCacheMax)
	// HTTP/2 drops the HTTP/1 server write timeout, so bound each write and
	// the whole response here; the controller is built on the original writer
	// because assetStatusWriter does not unwrap.
	writer := &deadlineWriter{
		ResponseWriter: w,
		controller:     http.NewResponseController(w),
		timeout:        h.writeTimeout,
		rest:           time.Now().Add(assetWriteTimeout),
		metrics:        h.metrics,
	}
	recorder := &assetStatusWriter{ResponseWriter: writer, status: code}
	// ServeContent adds Range/If-Range support and Content-Length; the digest
	// path remains the only addressable name.
	http.ServeContent(recorder, r, name, time.Time{}, bytes.NewReader(h.data))
	// Write the last buffered chunk under the deadline too; otherwise it is
	// flushed after the handler returns.
	writer.Flush()
	code = recorder.status
}

type assetStatusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader records and forwards the response status.
func (w *assetStatusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
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
