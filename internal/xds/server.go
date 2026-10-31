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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionconfigservice "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	xdsserver "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	authorizationHeader        = "authorization"
	invalidSubscriptionMessage = "invalid subscription"
	// retryVersionSuffix marks a republished configuration.
	retryVersionSuffix = ".retry"

	// nackRetryMaxDelay caps the exponential NACK backoff. The cap exists so
	// a persistently rejected configuration keeps a steady retry cadence
	// instead of stalling; there is no attempt or time bound because giving
	// up strands a fail-closed proxy.
	nackRetryMaxDelay = 30 * time.Second
	// authenticationDeadline bounds an unauthenticated stream that never sends
	// its first request, so idle clients cannot occupy every stream slot.
	authenticationDeadline = 10 * time.Second
)

// nackState tracks retries for one stream's current version.
type nackState struct {
	version  string
	attempts int
}

// nackKey scopes a retry budget to one stream. Every replica of a Gateway
// subscribes to the same resource name, so a budget keyed by resource alone
// lets one replica's exhausted budget block every other replica's retries.
func nackKey(resourceName string, sessionID uint64) string {
	return fmt.Sprintf("%s/%d", resourceName, sessionID)
}

// retryBase strips the retry suffix from a version.
func retryBase(version string) string {
	if i := strings.Index(version, retryVersionSuffix); i > 0 {
		return version[:i]
	}
	return version
}

// retryAttempt returns the retry number encoded in a version, or 0 for an
// accepted version.
func retryAttempt(version string) int {
	i := strings.Index(version, retryVersionSuffix)
	if i <= 0 {
		return 0
	}
	attempt, err := strconv.Atoi(version[i+len(retryVersionSuffix):])
	if err != nil || attempt < 0 {
		return 0
	}
	return attempt
}

// lifecycleKey carries the stream's cancel function from the interceptor into
// the go-control-plane callbacks, which have no other way to close a stream.
type lifecycleKey struct{}

type serverStream struct {
	ctx          context.Context
	identity     Identity
	session      *StreamSession
	resourceName string
	engineKey    string

	mu            sync.Mutex
	lastVersion   string
	lastNonce     string
	nackKey       string
	authenticated atomic.Bool
	authTimer     *time.Timer
}

// Server serves ECDS with go-control-plane's SnapshotCache and stream handler.
// Authentication, authorization, revalidation, and stream lifecycle remain
// CKO-owned.
type Server struct {
	Auth    *Authenticator
	Authz   *AuthzIndex
	Streams *StreamRegistry
	Limits  Limits
	Metrics *Metrics
	Store   *SnapshotStore
	Logger  logr.Logger

	// RetryDelay gives a rejected configuration's remote module fetch time to
	// finish before the proxy is asked to reapply it.
	RetryDelay time.Duration

	srv xdsserver.Server

	streams     sync.Map // int64 -> *serverStream
	nacks       sync.Map // nackKey(resource, session) -> nackState
	openStreams atomic.Int64
}

// NewServer wires the go-control-plane server around the snapshot cache.
func NewServer(store *SnapshotStore, auth *Authenticator, authz *AuthzIndex, streams *StreamRegistry, limits Limits, metrics *Metrics) *Server {
	s := &Server{
		Auth:       auth,
		Authz:      authz,
		Streams:    streams,
		Limits:     limits.WithDefaults(),
		Metrics:    metrics,
		Store:      store,
		Logger:     logr.Discard(),
		RetryDelay: 2 * time.Second,
	}
	s.srv = xdsserver.NewServer(context.Background(), store.cache, xdsserver.CallbackFuncs{
		StreamOpenFunc:     s.onStreamOpen,
		StreamRequestFunc:  s.onStreamRequest,
		StreamResponseFunc: s.onStreamResponse,
		StreamClosedFunc:   s.onStreamClosed,
	})
	return s
}

// streamOnlyECDS registers only the authenticated state-of-the-world stream.
// The generated ECDS service also exposes unary and delta RPCs; those would
// bypass this server's stream interceptor, authentication, authorization and
// node.id pinning, so they fail explicitly instead of serving snapshots.
type streamOnlyECDS struct {
	extensionconfigservice.UnimplementedExtensionConfigDiscoveryServiceServer
	srv extensionconfigservice.ExtensionConfigDiscoveryServiceServer
}

// StreamExtensionConfigs delegates the supported state-of-the-world stream.
func (s streamOnlyECDS) StreamExtensionConfigs(stream extensionconfigservice.ExtensionConfigDiscoveryService_StreamExtensionConfigsServer) error {
	return s.srv.StreamExtensionConfigs(stream)
}

// Register registers the ECDS service on the gRPC server.
func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	extensionconfigservice.RegisterExtensionConfigDiscoveryServiceServer(registrar, streamOnlyECDS{srv: s.srv})
}

// lifecycleStatus maps a stream close cause to the bounded gRPC status
// returned to the client, or nil when the cause is not lifecycle-owned.
func lifecycleStatus(cause error) error {
	switch cause {
	case ErrRevoked:
		return status.Error(codes.PermissionDenied, "resource is not authorized")
	case ErrRevalidateFailed:
		return status.Error(codes.Unauthenticated, "authentication failed")
	case ErrLifetimeExpired, ErrServerShutdown, ErrResubscribe:
		return status.Error(codes.Unavailable, "stream must be re-established")
	}
	return nil
}

// StreamInterceptor supplies the cancelable stream context the revalidation
// registry needs. go-control-plane has no public close-stream API.
func (s *Server) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, cancel := context.WithCancelCause(stream.Context())

		wrapped := &streamWithContext{
			ServerStream: stream,
			ctx:          context.WithValue(ctx, lifecycleKey{}, cancel),
		}
		err := handler(srv, wrapped)
		if err != nil || ctx.Err() != nil {
			s.Logger.V(1).Info("ECDS stream ended", "err", err, "cause", context.Cause(ctx))
		}
		if ctx.Err() != nil {
			if mapped := lifecycleStatus(context.Cause(ctx)); mapped != nil {
				return mapped
			}
		}
		if err == nil {
			return nil
		}
		// Preserve explicit auth/authz/subscription errors. Only map errors that
		// come from the lifecycle cancellation below.
		if st, ok := status.FromError(err); ok && st.Code() != codes.Canceled && st.Code() != codes.Unknown {
			return err
		}
		if ctx.Err() != nil {
			if mapped := lifecycleStatus(context.Cause(ctx)); mapped != nil {
				return mapped
			}
		}
		return err
	}
}

type streamWithContext struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the stream lifecycle context.
func (s *streamWithContext) Context() context.Context { return s.ctx }

// RecvMsg observes the lifecycle context. go-control-plane's stream handler
// blocks in Recv, so cancellation must unblock it here; the read goroutine ends
// when the server closes the stream.
func (s *streamWithContext) RecvMsg(m any) error {
	done := make(chan error, 1)
	go func() { done <- s.ServerStream.RecvMsg(m) }()
	select {
	case err := <-done:
		return err
	case <-s.ctx.Done():
		return status.Error(codes.Canceled, "stream closed")
	}
}

// SendMsg observes the lifecycle context.
func (s *streamWithContext) SendMsg(m any) error {
	if s.ctx.Err() != nil {
		return status.Error(codes.Canceled, "stream closed")
	}
	return s.ServerStream.SendMsg(m)
}

func (s *Server) onStreamOpen(ctx context.Context, streamID int64, _ string) error {
	// The cap must hold at open time: a client can hold streams open without
	// ever sending a request, which RegisterWithCancel would never see.
	if s.openStreams.Add(1) > int64(s.Limits.MaxStreams) {
		s.openStreams.Add(-1)
		s.Metrics.StreamLimitRejected()
		return status.Error(codes.ResourceExhausted, "stream limit exceeded")
	}
	// Authentication runs on the first request, after the stream is fully
	// established, so an auth failure is delivered as a gRPC status rather
	// than an early stream reset. Bound how long a stream may stay idle before
	// its first request so it cannot reserve a slot indefinitely.
	entry := &serverStream{ctx: ctx}
	if cancel, ok := ctx.Value(lifecycleKey{}).(context.CancelCauseFunc); ok {
		entry.authTimer = time.AfterFunc(authenticationDeadline, func() {
			if !entry.authenticated.Load() {
				cancel(ErrRevalidateFailed)
			}
		})
	}
	s.streams.Store(streamID, entry)
	return nil
}

func (s *Server) onStreamRequest(streamID int64, req *discoveryv3.DiscoveryRequest) error {
	raw, ok := s.streams.Load(streamID)
	if !ok {
		return status.Error(codes.Unauthenticated, "authentication failed")
	}
	entry := raw.(*serverStream)

	if entry.session == nil {
		token, err := bearerToken(entry.ctx)
		if err != nil {
			s.Metrics.AuthFailure("error")
			return status.Error(codes.Unauthenticated, "authentication failed")
		}
		identity, err := s.Auth.Authenticate(entry.ctx, token)
		if err != nil {
			s.Metrics.AuthFailure("error")
			return status.Error(codes.Unauthenticated, "authentication failed")
		}
		cancel, _ := entry.ctx.Value(lifecycleKey{}).(context.CancelCauseFunc)
		session, err := s.Streams.RegisterWithCancel(entry.ctx, cancel, "", *identity, token)
		if err != nil {
			s.Metrics.StreamLimitRejected()
			return status.Error(codes.ResourceExhausted, "stream limit exceeded")
		}
		s.Metrics.AuthSuccess()
		entry.identity = *identity
		entry.session = session
		entry.authenticated.Store(true)
		entry.mu.Lock()
		if entry.authTimer != nil {
			entry.authTimer.Stop()
		}
		entry.mu.Unlock()
	}

	// The client controls node.id. Pin the cache key on every request,
	// including ACKs/NACKs that may arrive before a subscription. Before an
	// authorized subscription exists, the empty key cannot match a snapshot:
	// namespacedNameKey always contains "/".
	if req.Node == nil {
		req.Node = &corev3.Node{}
	}
	req.Node.Id = entry.engineKey

	if req.GetErrorDetail() != nil {
		s.Metrics.Nack("rejected")
		// A proxy that rejects a configuration because the remote module is not
		// cached yet only re-applies it on a new subscription, so end the
		// stream with a retryable status. Envoy can send the NACK as the first
		// message of a reconnected stream, before any subscription was recorded
		// and possibly without resource_names. The resource is still
		// client-controlled, so authorize it before it can change a retry
		// version or consume a retry budget.
		rejected := s.rejectedResourceName(entry, req)
		if rejected == "" {
			s.Logger.Info("ECDS NACK without a resolvable resource",
				"session", entry.session.ID, "nonce", req.GetResponseNonce(), "version", req.GetVersionInfo())
			return nil
		}
		if !s.Authz.Authorize(rejected, entry.identity) {
			s.Metrics.AuthFailure("unauthorized")
			s.Logger.Info("ECDS rejected resource not authorized", "resource", rejected, "identity", entry.identity.Key(), "uid", entry.identity.UID)
			return status.Error(codes.Unavailable, "authorization is not ready")
		}
		// A NACK's version_info is the last version the proxy ACCEPTED, not the
		// version it rejected. Prefer the last response on this stream and fall
		// back to the currently published snapshot.
		s.resubscribeRejected(entry, rejected, s.rejectedVersion(entry, req, rejected))
		return nil
	}

	names := req.GetResourceNames()
	if len(names) == 0 {
		if req.GetResponseNonce() != "" {
			s.Metrics.Ack()
			return nil
		}
		return status.Error(codes.InvalidArgument, invalidSubscriptionMessage)
	}
	if len(names) != 1 {
		return status.Error(codes.InvalidArgument, invalidSubscriptionMessage)
	}

	name := names[0]
	if name == "" || !strings.HasPrefix(name, resourceNamePrefix) || strings.ContainsAny(name, "*?") {
		return status.Error(codes.InvalidArgument, invalidSubscriptionMessage)
	}
	if name != entry.resourceName {
		// Record the subscription before authorizing so a concurrent
		// RevokeResource can find and cancel this session during setup. A
		// failed authorization ends the stream, so the entry is never reused.
		entry.resourceName = name
		s.Streams.SetResource(entry.session.ID, name)
		// Authorization is retryable: the mapping is rebuilt on Engine and pod
		// events, and Envoy treats PermissionDenied as terminal for a
		// subscription, which would leave the filter fail-closed for good.
		if !s.Authz.Authorize(name, entry.identity) {
			s.Metrics.AuthFailure("unauthorized")
			s.Logger.Info("ECDS subscription not authorized yet", "resource", name, "identity", entry.identity.Key(), "uid", entry.identity.UID)
			return status.Error(codes.Unavailable, "authorization is not ready")
		}
		engineKey, ok := s.Authz.EngineKey(name)
		if !ok {
			s.Metrics.AuthFailure("unauthorized")
			return status.Error(codes.Unavailable, "authorization is not ready")
		}
		// The cache is keyed by node.id, which the client controls. Rewrite it
		// to the authorized Engine key before CreateWatch runs.
		if req.Node == nil {
			req.Node = &corev3.Node{}
		}
		req.Node.Id = engineKey
		entry.engineKey = engineKey
		return nil
	}

	if req.GetResponseNonce() != "" {
		s.Metrics.Ack()
		s.clearNack(entry)
		s.Logger.Info("ECDS configuration accepted by proxy", "resource", entry.resourceName, "version", req.GetVersionInfo())
	}
	return nil
}

// resubscribeRejected reports whether the rejected configuration should end the
// stream so the proxy resubscribes. A cold proxy rejects the first push because
// the remote module is not cached yet, and Envoy never asks for that resource
// again on its own; the reconnect gives it the configuration once the fetch has
// completed.
// rejectedResourceName returns the resource a NACK refers to. Envoy may send
// it before the stream recorded its subscription, and a NACK replayed on a
// reconnected stream may carry no resource_names at all: fall back to the
// only resource the identity is authorized for, so the retry ladder cannot be
// dropped silently.
func (s *Server) rejectedResourceName(entry *serverStream, req *discoveryv3.DiscoveryRequest) string {
	if entry.resourceName != "" {
		return entry.resourceName
	}
	if names := req.GetResourceNames(); len(names) == 1 {
		if name := names[0]; strings.HasPrefix(name, resourceNamePrefix) && !strings.ContainsAny(name, "*?") {
			return name
		}
	}
	if len(req.GetResourceNames()) == 0 {
		return s.Authz.soleResource(entry.identity)
	}
	return ""
}

// resubscribeRejected republishes the rejected configuration under a derived
// version and ends the stream so the proxy resubscribes. A cold proxy rejects
// the first push because the remote module is not cached yet, and Envoy never
// asks for that resource again on its own; the reconnect gives it the
// configuration once the fetch has completed.
func (s *Server) resubscribeRejected(entry *serverStream, resourceName, rejectedVersion string) {
	if resourceName == "" || s.Store == nil || entry.session == nil {
		return
	}
	// The retry ladder is the live snapshot's version suffix, so every
	// rejection advances it monotonically: concurrent replicas rejecting the
	// same version republish the same derived version instead of racing the
	// ladder upwards, and a proxy that reconnects gets the next rung.
	version := rejectedVersion
	if current, err := s.Store.CurrentVersion(resourceName); err == nil {
		version = current
	}
	base := retryBase(version)
	attempt := retryAttempt(version) + 1

	// The budget is per stream: replicas of a Gateway must not spend each
	// other's attempts. Retries of the same content carry a derived version;
	// count them against the version the Engine accepted. Giving up is not an
	// option: a stranded pod stays fail-closed forever, and the entry is
	// reclaimed on ACK and stream close.
	key := nackKey(resourceName, entry.session.ID)
	s.trackNack(entry, key)
	state := s.recordNack(key, base)
	s.Logger.Info("resubscribing proxy after rejected configuration",
		"resource", resourceName, "version", rejectedVersion, "attempt", attempt, "rejections", state.attempts)

	// Envoy never re-applies a version it rejected, and the remote module fetch
	// it started needs to finish first: publish a derived version and close the
	// stream so the proxy resubscribes and applies it from the code cache.
	time.AfterFunc(nackDelay(s.RetryDelay, attempt), func() {
		if err := s.Store.BumpRejected(resourceName, base, attempt); err != nil {
			s.Logger.Error(err, "republish rejected configuration", "resource", resourceName)
			return
		}
		s.Streams.ResetStream(entry.session.ID)
	})
}

// nackDelay doubles the base delay for every retry rung of the same content,
// starting at the configured RetryDelay. A rung, not the per-stream budget,
// sets the backoff: each retry closes the stream, so only the published
// version survives a reconnect and can remember how often the content was
// rejected.
func nackDelay(base time.Duration, attempt int) time.Duration {
	if base <= 0 || attempt <= 1 {
		return base
	}
	delay := base
	for range attempt - 1 {
		delay *= 2
		if delay >= nackRetryMaxDelay {
			return nackRetryMaxDelay
		}
	}
	return delay
}

// trackNack records the stream's budget entry so its ACK and close can reclaim
// it. A stream budgets one resource at a time; switching drops the previous
// entry instead of leaking it.
func (s *Server) trackNack(entry *serverStream, key string) {
	entry.mu.Lock()
	previous := entry.nackKey
	entry.nackKey = key
	entry.mu.Unlock()
	if previous != "" && previous != key {
		s.nacks.Delete(previous)
	}
}

// clearNack drops the stream's budget entry on ACK or stream close so closed
// streams cannot leak budget entries.
func (s *Server) clearNack(entry *serverStream) {
	entry.mu.Lock()
	key := entry.nackKey
	entry.nackKey = ""
	entry.mu.Unlock()
	if key != "" {
		s.nacks.Delete(key)
	}
}

// recordNack increments the retry attempt for key without losing concurrent
// updates. Sync.Map has no atomic read-modify-write, so compare and swap in a
// loop.
func (s *Server) recordNack(key, version string) nackState {
	for {
		prev, ok := s.nacks.Load(key)
		if !ok {
			state := nackState{version: version, attempts: 1}
			actual, loaded := s.nacks.LoadOrStore(key, state)
			if !loaded {
				return state
			}
			prev = actual
		}
		current := prev.(nackState)
		state := nackState{version: version}
		if current.version == version {
			state = current
		}
		state.attempts++
		if s.nacks.CompareAndSwap(key, prev, state) {
			return state
		}
	}
}

// onStreamResponse records the version and nonce last sent on the stream. A
// NACK identifies that response by nonce; its version_info is the previously
// accepted version, so it cannot identify the rejected content by itself.
func (s *Server) onStreamResponse(_ context.Context, streamID int64, _ *discoveryv3.DiscoveryRequest, resp *discoveryv3.DiscoveryResponse) {
	raw, ok := s.streams.Load(streamID)
	if !ok {
		return
	}
	entry := raw.(*serverStream)
	entry.mu.Lock()
	entry.lastVersion = resp.GetVersionInfo()
	entry.lastNonce = resp.GetNonce()
	entry.mu.Unlock()
}

// rejectedVersion returns the version the proxy is rejecting. Envoy sends the
// previous accepted version in version_info, so use the response nonce first and
// fall back to the live snapshot for a NACK replayed on a fresh stream.
func (s *Server) rejectedVersion(entry *serverStream, req *discoveryv3.DiscoveryRequest, resourceName string) string {
	entry.mu.Lock()
	lastVersion, lastNonce := entry.lastVersion, entry.lastNonce
	entry.mu.Unlock()
	if lastVersion != "" && lastNonce != "" && req.GetResponseNonce() == lastNonce {
		return lastVersion
	}
	// This store read on the request path relies on the cache not blocking in a
	// response send; re-check that assumption when go-control-plane is bumped.
	if s.Store == nil || resourceName == "" {
		return ""
	}
	version, err := s.Store.CurrentVersion(resourceName)
	if err != nil {
		return ""
	}
	return version
}

func (s *Server) onStreamClosed(streamID int64, _ *corev3.Node) {
	raw, ok := s.streams.LoadAndDelete(streamID)
	if !ok {
		// Rejected opens also trigger the closed callback; their slot was
		// already rolled back.
		return
	}
	s.openStreams.Add(-1)
	entry := raw.(*serverStream)
	entry.mu.Lock()
	if entry.authTimer != nil {
		entry.authTimer.Stop()
	}
	entry.mu.Unlock()
	s.clearNack(entry)
	if entry.session != nil {
		s.Streams.Unregister(entry.session.ID)
	}
}

// bearerToken extracts exactly one "Bearer <token>" authorization header.
func bearerToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", errors.New("missing gRPC metadata")
	}
	values := md.Get(authorizationHeader)
	if len(values) != 1 {
		return "", errors.New("missing or duplicate authorization header")
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, bearerScheme) || strings.TrimSpace(token) == "" {
		return "", errors.New("malformed authorization header")
	}
	token = strings.TrimSpace(token)
	if len(token) > maxBearerTokenLen {
		return "", fmt.Errorf("bearer token exceeds %d bytes", maxBearerTokenLen)
	}
	return token, nil
}
