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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionconfigservice "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	xdsserver "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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
	// authenticationDeadline bounds a stream that never sends its first
	// request, so idle clients cannot occupy every stream slot.
	authenticationDeadline = 10 * time.Second
	// podStreamCap bounds concurrent streams per Gateway pod. Envoy keeps one
	// ECDS stream per pod; the rest covers reconnect overlap. A ServiceAccount
	// may hold podStreamCap streams per authorized pod running as it, plus
	// identityStreamSlack for pods the authorization index has not counted yet.
	podStreamCap        = 4
	identityStreamSlack = 16
	// authenticationShare sizes, as a share of MaxStreams, the pool of
	// requests waiting for their token review and the cap on token reviews
	// for new streams in flight.
	authenticationShare = 4
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

func proxyCacheKey(engineKey string, identity Identity, proxyID string) string {
	if proxyID == "" {
		// shortcut: clients without a stable Node.Id share retries by identity; require Node.Id to isolate them.
		proxyID = identity.Key() + "/" + identity.UID
	}
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%s", engineKey, identity.Key(), identity.UID, proxyID)
	sum := sha256.Sum256([]byte(key))
	return engineKey + "\x00proxy:" + hex.EncodeToString(sum[:])
}

func (s *Server) proxyCacheKeyFor(entry *serverStream, resourceName string) string {
	engineKey := entry.engineKey
	if engineKey == "" {
		var ok bool
		engineKey, ok = s.Authz.EngineKey(resourceName)
		if !ok {
			return ""
		}
	}
	if entry.proxyKey == "" || entry.engineKey != engineKey {
		entry.engineKey = engineKey
		entry.proxyKey = proxyCacheKey(engineKey, entry.identity, entry.proxyID)
	}
	return entry.proxyKey
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

// admissionKey carries the listener's streamAdmission into the stream
// callbacks through the request context.
type admissionKey struct{}

// streamAdmission is one ECDS request the listener authenticated before
// reading its body, and the slots it holds until it is released.
type streamAdmission struct {
	identity Identity
	token    string
	// writer is the request's response writer, set before gRPC serves it.
	writer *deadlineWriter
	// body is the request's body, set before gRPC serves it.
	body *requestBody

	mu        sync.Mutex
	sessionID uint64
	released  bool
}

// bind records the request's registry session. It reports false when the
// request has already been released, in which case the caller unregisters.
func (a *streamAdmission) bind(sessionID uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released {
		return false
	}
	a.sessionID = sessionID
	return true
}

type serverStream struct {
	ctx          context.Context
	streamID     int64
	identity     Identity
	admission    *streamAdmission
	session      *StreamSession
	resourceName string
	engineKey    string
	proxyID      string
	proxyKey     string
	proxyBound   bool
	// ackedVersion is the version the proxy last acknowledged on this stream.
	ackedVersion string

	mu          sync.Mutex
	lastVersion string
	lastNonce   string
	nackKey     string
	requested   atomic.Bool
	authTimer   *time.Timer
	// retryPending is true while a rejected-configuration retry is scheduled.
	// retryTimer is that retry, kept so ACK and stream close can cancel it.
	retryPending bool
	retryTimer   *time.Timer
	closed       bool
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
	// authDeadline bounds a stream that never sends a request. It defaults to
	// authenticationDeadline and is a field so tests can shorten it.
	authDeadline time.Duration

	srv xdsserver.Server

	streams   sync.Map // int64 -> *serverStream
	nacks     sync.Map // nackKey(resource, session) -> nackState
	responses *responseBudget
	// authSlots holds one token per request waiting for its token review.
	authSlots chan struct{}
	// permits holds one token per token review for a new stream in
	// flight, also after its request ended. Revalidation has its own bounded
	// workers and takes none.
	permits chan struct{}

	// openStreams counts admitted streams; perIdentity, perNamespace and
	// perPod partition them. The maps are guarded by countsMu.
	openStreams  atomic.Int64
	countsMu     sync.Mutex
	perIdentity  map[string]int
	perNamespace map[string]int
	perPod       map[string]int
}

// NewServer wires the go-control-plane server around the snapshot cache.
func NewServer(store *SnapshotStore, auth *Authenticator, authz *AuthzIndex, streams *StreamRegistry, limits Limits, metrics *Metrics) *Server {
	limits = limits.WithDefaults()
	if auth != nil {
		auth.failureLimiter = newAuthFailureLimiter(limits.AuthFailureRate, limits.AuthFailureBurst)
	}
	s := &Server{
		Auth:         auth,
		Authz:        authz,
		Streams:      streams,
		Limits:       limits,
		Metrics:      metrics,
		Store:        store,
		Logger:       logr.Discard(),
		RetryDelay:   2 * time.Second,
		authDeadline: authenticationDeadline,
		responses:    newResponseBudget(),
		authSlots:    make(chan struct{}, max(1, limits.MaxStreams/authenticationShare)),
		permits:      make(chan struct{}, max(1, limits.MaxStreams/authenticationShare)),
		perIdentity:  make(map[string]int),
		perNamespace: make(map[string]int),
		perPod:       make(map[string]int),
	}
	s.srv = xdsserver.NewServer(context.Background(), store.cache, xdsserver.CallbackFuncs{
		StreamOpenFunc:     s.onStreamOpen,
		StreamRequestFunc:  s.onStreamRequest,
		StreamResponseFunc: s.onStreamResponse,
		StreamClosedFunc:   s.onStreamClosed,
	})
	return s
}

// ecdsStreamMethod is the only gRPC method the listener serves.
const ecdsStreamMethod = extensionconfigservice.ExtensionConfigDiscoveryService_StreamExtensionConfigs_FullMethodName

// ecdsStreamService registers only the authenticated state-of-the-world
// stream. The generated ECDS service also has unary and delta methods, which
// would decode a request outside this server's stream lifecycle, so they are
// not registered.
var ecdsStreamService = grpc.ServiceDesc{
	ServiceName: extensionconfigservice.ExtensionConfigDiscoveryService_ServiceDesc.ServiceName,
	HandlerType: (*extensionconfigservice.ExtensionConfigDiscoveryServiceServer)(nil),
	Streams: []grpc.StreamDesc{{
		StreamName: "StreamExtensionConfigs",
		Handler: func(srv any, stream grpc.ServerStream) error {
			return srv.(extensionconfigservice.ExtensionConfigDiscoveryServiceServer).StreamExtensionConfigs(
				&grpc.GenericServerStream[discoveryv3.DiscoveryRequest, discoveryv3.DiscoveryResponse]{ServerStream: stream})
		},
		ServerStreams: true,
		ClientStreams: true,
	}},
	Metadata: extensionconfigservice.ExtensionConfigDiscoveryService_ServiceDesc.Metadata,
}

// Register registers the ECDS stream on the gRPC server.
func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	registrar.RegisterService(&ecdsStreamService, s.srv)
}

// admit authenticates an ECDS request from its headers, before any of its
// body is read, and reserves its stream slots. A non-nil admission must be
// released; see release. review marks the request's connection while its
// token is under review and returns the function that unmarks it.
func (s *Server) admit(r *http.Request, review func() (end func())) (*streamAdmission, error) {
	token, err := bearerToken(r.Header.Values(authorizationHeader))
	if err != nil {
		s.Metrics.AuthFailure("error")
		return nil, status.Error(codes.Unauthenticated, "authentication failed")
	}
	identity, err := s.authenticate(r, token, review)
	if err != nil {
		return nil, err
	}
	if err := s.reserve(*identity); err != nil {
		return nil, err
	}
	s.Metrics.AuthSuccess()
	return &streamAdmission{identity: *identity, token: token}, nil
}

// authenticate validates token. A token without a cached result is refused
// at once when its source's failure budget is spent; otherwise it waits, for
// at most the review timeout in all, first for one of a fixed number of
// authentication slots and then for a review permit, so a reconnect burst
// queues instead of being refused; a cached token waits for neither. A
// request that ends frees its slot at once, while its review finishes on its
// own within the review timeout, keeps its permit until then and still
// charges a rejected token to its source, so token reviews in flight stay
// capped even when requests are reset. A slot also marks the request with
// review, which protects its connection from being closed for room.
func (s *Server) authenticate(r *http.Request, token string, review func() (end func())) (*Identity, error) {
	ctx := requestPeerContext(r)
	// A cached identity is used as found: a second lookup could miss and
	// review without a slot or permit.
	if identity, ok := s.Auth.cached(token); ok {
		return identity, nil
	}
	// Only a peek: a permit taken here would count as a review in flight for
	// the whole wait, and refuse valid tokens queued behind it.
	if !s.Auth.failureLimiter.allows(ctx) {
		return nil, s.authenticationFailure(ErrAuthRateLimited)
	}
	wait := time.NewTimer(s.Limits.TokenReviewTimeout)
	defer wait.Stop()
	if err := s.acquire(s.authSlots, wait.C, r); err != nil {
		return nil, err
	}
	defer func() { <-s.authSlots }()
	defer review()()
	if err := s.acquire(s.permits, wait.C, r); err != nil {
		return nil, err
	}
	done := make(chan tokenReviewResult, 1)
	go func() {
		defer func() { <-s.permits }()
		identity, err := s.Auth.Authenticate(ctx, token)
		done <- tokenReviewResult{identity: identity, err: err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			return nil, s.authenticationFailure(result.err)
		}
		return result.identity, nil
	case <-r.Context().Done():
		return nil, status.Error(codes.Canceled, "request canceled")
	}
}

// acquire takes a token from pool, waiting until wait fires or the request
// ends.
func (s *Server) acquire(pool chan struct{}, wait <-chan time.Time, r *http.Request) error {
	select {
	case pool <- struct{}{}:
		return nil
	case <-wait:
		s.Metrics.Refused(RefusalAuthenticationQueue)
		return status.Error(codes.ResourceExhausted, "authentication is busy")
	case <-r.Context().Done():
		return status.Error(codes.Canceled, "request canceled")
	}
}

// reserve takes a stream slot for id: one of MaxStreams, at most podStreamCap
// for its pod, for its ServiceAccount at most podStreamCap per authorized pod
// plus identityStreamSlack, never more than MaxStreamsPerIdentity, and for all
// identities of its namespace together podStreamCap per pod counted there plus
// identityStreamSlack, so the slack does not multiply across ServiceAccounts.
// An identity no Engine grants is refused, so it cannot hold slots a Gateway
// needs.
func (s *Server) reserve(id Identity) error {
	share, namespacePods, granted := s.Authz.podShare(id)
	if !granted {
		s.Metrics.Refused(RefusalUnauthorized)
		return status.Error(codes.Unavailable, "authorization is not ready")
	}
	key := id.Key()
	identityCap := min(s.Limits.MaxStreamsPerIdentity, podStreamCap*share+identityStreamSlack)
	namespaceCap := podStreamCap*namespacePods + identityStreamSlack
	s.countsMu.Lock()
	defer s.countsMu.Unlock()
	if s.openStreams.Load() >= int64(s.Limits.MaxStreams) || s.perIdentity[key] >= identityCap ||
		s.perNamespace[id.Namespace] >= namespaceCap || (id.PodUID != "" && s.perPod[id.PodUID] >= podStreamCap) {
		s.Metrics.StreamLimitRejected()
		return status.Error(codes.ResourceExhausted, "stream limit exceeded")
	}
	s.openStreams.Add(1)
	s.perIdentity[key]++
	s.perNamespace[id.Namespace]++
	if id.PodUID != "" {
		s.perPod[id.PodUID]++
	}
	return nil
}

func (s *Server) unreserve(id Identity) {
	s.countsMu.Lock()
	defer s.countsMu.Unlock()
	s.openStreams.Add(-1)
	decrement(s.perIdentity, id.Key())
	decrement(s.perNamespace, id.Namespace)
	if id.PodUID != "" {
		decrement(s.perPod, id.PodUID)
	}
}

func decrement(counts map[string]int, key string) {
	if counts[key] > 1 {
		counts[key]--
	} else {
		delete(counts, key)
	}
}

// release frees the slots an admitted request holds when its HTTP handler
// returns. They are kept after the stream's handler ends until any response
// still being written is done or aborted, so a stream cannot leave responses
// in flight beyond its caps; a stalled connection ends at its write timeout.
func (s *Server) release(admission *streamAdmission) {
	admission.mu.Lock()
	admission.released = true
	sessionID := admission.sessionID
	admission.mu.Unlock()
	s.unreserve(admission.identity)
	if sessionID != 0 {
		s.Streams.Unregister(sessionID)
	}
}

// requestPeerContext exposes the request's source address to the per-source
// authentication failure limiter.
func requestPeerContext(r *http.Request) context.Context {
	address, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.Context()
	}
	return peer.NewContext(r.Context(), &peer.Peer{Addr: net.TCPAddrFromAddrPort(address)})
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
		// A stream that ends while its response is written, because its
		// handler returned or was closed, for example by a revocation while
		// it waits behind that write, ends the write at once. It then ends
		// with RST_STREAM rather than its status, such as PermissionDenied;
		// Envoy retries and admission refuses the retry.
		wrapped := &streamWithContext{
			ServerStream: stream,
			ctx:          context.WithValue(ctx, lifecycleKey{}, cancel),
		}
		if admission, ok := ctx.Value(admissionKey{}).(*streamAdmission); ok {
			stop := context.AfterFunc(ctx, admission.writer.abort)
			defer func() {
				stop()
				admission.writer.abort()
			}()
			wrapped.body = admission.body
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
	// body, set for an admitted request, reads one more message for each the
	// handler receives.
	body *requestBody
}

// Context returns the stream lifecycle context.
func (s *streamWithContext) Context() context.Context { return s.ctx }

// RecvMsg observes the lifecycle context. go-control-plane's stream handler
// blocks in Recv, so cancellation must unblock it here; the read goroutine ends
// when the server closes the stream.
func (s *streamWithContext) RecvMsg(m any) error {
	done := make(chan error, 1)
	go func() {
		err := s.ServerStream.RecvMsg(m)
		if err == nil && s.body != nil {
			s.body.received()
		}
		done <- err
	}()
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
	// The listener authenticated the request before handing it to gRPC; a
	// stream without an admission did not come through it.
	admission, ok := ctx.Value(admissionKey{}).(*streamAdmission)
	if !ok {
		s.Metrics.AuthFailure("error")
		return status.Error(codes.Unauthenticated, "authentication failed")
	}
	// Bound how long a stream may stay idle before its first request so it
	// cannot reserve an open-stream slot indefinitely.
	entry := &serverStream{ctx: ctx, streamID: streamID, identity: admission.identity, admission: admission}
	deadline := s.authDeadline
	if deadline <= 0 {
		deadline = authenticationDeadline
	}
	if cancel, ok := ctx.Value(lifecycleKey{}).(context.CancelCauseFunc); ok {
		entry.authTimer = time.AfterFunc(deadline, func() {
			entry.mu.Lock()
			idle := !entry.requested.Load()
			entry.mu.Unlock()
			if idle {
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

	if !entry.requested.Load() {
		// The deadline bounds a stream that never sends a request: one arrived.
		entry.mu.Lock()
		if entry.authTimer != nil {
			entry.authTimer.Stop()
		}
		entry.requested.Store(true)
		entry.mu.Unlock()
	}
	if err := s.checkRequest(req); err != nil {
		return err
	}
	pinRequestNode(entry, req)

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
			return s.invalidRequest()
		}
		if err := s.authorizeStream(entry, rejected); err != nil {
			return err
		}
		if err := s.spendResponse(entry); err != nil {
			return err
		}
		req.ResourceNames = []string{rejected}
		// A NACK's version_info is the last version the proxy ACCEPTED, not the
		// version it rejected. Prefer the last response on this stream and fall
		// back to the currently published snapshot.
		s.resubscribeRejected(entry, rejected, s.rejectedVersion(entry, req, rejected))
		return nil
	}

	// An ACK of the last response must not make the cache resend it, whatever
	// version_info the client claims.
	entry.mu.Lock()
	lastVersion := entry.lastVersion
	if nonce := req.GetResponseNonce(); nonce != "" && nonce == entry.lastNonce {
		req.VersionInfo = lastVersion
	}
	entry.mu.Unlock()

	// Before a subscription exists only a named subscription is accepted; later
	// requests may omit the name they already subscribed to.
	name := entry.resourceName
	switch names := req.GetResourceNames(); {
	case len(names) == 1 && validResourceName(names[0]):
		name = names[0]
	case len(names) != 0 || name == "":
		return s.invalidRequest()
	}
	subscribing := name != entry.resourceName
	// Authorization is retryable: the mapping is rebuilt on Engine and pod
	// events, and Envoy treats PermissionDenied as terminal for a
	// subscription, which would leave the filter fail-closed for good.
	if err := s.authorizeStream(entry, name); err != nil {
		return err
	}
	req.ResourceNames = []string{name}
	// The cache answers with the full resource unless the request carries the
	// version last sent on this stream.
	if subscribing || req.GetResponseNonce() == "" || lastVersion == "" || req.GetVersionInfo() != lastVersion {
		if err := s.spendResponse(entry); err != nil {
			return err
		}
	}
	if subscribing {
		engineKey, ok := s.Authz.EngineKey(name)
		if !ok {
			s.Metrics.AuthFailure("unauthorized")
			return status.Error(codes.Unavailable, "authorization is not ready")
		}
		req.Node.Id = s.watchNode(entry, name, engineKey, req.GetVersionInfo())
		return nil
	}

	if req.GetResponseNonce() != "" {
		s.Metrics.Ack()
		// Do not cancel a pending retry here: Envoy sends an ACK for the last
		// accepted version immediately after a cold-start NACK, and the retry
		// has not published its derived version yet. Only stream close cancels
		// it, so a rejected configuration is still republished.
		s.clearNack(entry)
		s.ackProxyRetry(entry, req.GetVersionInfo())
		// A proxy may repeat its acknowledgement; log each version once.
		if req.GetVersionInfo() != entry.ackedVersion {
			entry.ackedVersion = req.GetVersionInfo()
			s.Logger.Info("ECDS configuration accepted by proxy", "resource", entry.resourceName, "version", req.GetVersionInfo())
		}
	}
	req.Node.Id = s.watchNode(entry, name, entry.engineKey, req.GetVersionInfo())
	return nil
}

// Bounds on client-supplied request strings. Envoy echoes server-generated
// versions and nonces (under 100 bytes) and truncates its rejection message
// to 4096 characters; Istio node IDs and resource names stay under 1 KiB.
const (
	maxResourceNameBytes = 1 << 10
	maxVersionInfoBytes  = 2 << 10
	maxNonceBytes        = 2 << 10
	maxErrorMessageBytes = 8 << 10
	maxNodeIDBytes       = 1 << 10
)

// checkRequest refuses requests no Envoy sends and bounds the client-supplied
// strings the stream keeps or logs. A long rejection message is truncated
// rather than refused, so the rejection is still retried.
func (s *Server) checkRequest(req *discoveryv3.DiscoveryRequest) error {
	if req.GetTypeUrl() != resource.ExtensionConfigType ||
		len(req.GetVersionInfo()) > maxVersionInfoBytes || len(req.GetResponseNonce()) > maxNonceBytes {
		return s.invalidRequest()
	}
	if detail := req.GetErrorDetail(); detail != nil {
		detail.Message = truncateUTF8(detail.GetMessage(), maxErrorMessageBytes)
		detail.Details = nil
	}
	return nil
}

func (s *Server) invalidRequest() error {
	s.Metrics.Refused(RefusalInvalidRequest)
	return status.Error(codes.InvalidArgument, invalidSubscriptionMessage)
}

// validResourceName reports whether name has the shape of a CKO ECDS
// resource; authorization decides whether the stream may have it.
func validResourceName(name string) bool {
	return len(name) <= maxResourceNameBytes && strings.HasPrefix(name, resourceNamePrefix) && !strings.ContainsAny(name, "*?")
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// authorizeStream authorizes resourceName for the stream's identity on every
// request. The stream is registered only once a subscription is authorized;
// the check is repeated after it is visible to RevokeResource, so a
// revocation in between is not missed.
func (s *Server) authorizeStream(entry *serverStream, resourceName string) error {
	if !s.authorized(entry, resourceName) {
		return status.Error(codes.Unavailable, "authorization is not ready")
	}
	if entry.session != nil && entry.resourceName == resourceName {
		return nil
	}
	if entry.session == nil {
		cancel, _ := entry.ctx.Value(lifecycleKey{}).(context.CancelCauseFunc)
		session := s.Streams.RegisterWithCancel(entry.ctx, cancel, resourceName, entry.identity, entry.admission.token)
		if !entry.admission.bind(session.ID) {
			s.Streams.Unregister(session.ID)
			return status.Error(codes.Unavailable, "stream must be re-established")
		}
		entry.session = session
	} else {
		s.Streams.SetResource(entry.session.ID, resourceName)
	}
	entry.resourceName = resourceName
	if !s.authorized(entry, resourceName) {
		return status.Error(codes.Unavailable, "authorization is not ready")
	}
	return nil
}

func (s *Server) authorized(entry *serverStream, resourceName string) bool {
	if s.Authz.Authorize(resourceName, entry.identity) {
		return true
	}
	s.Metrics.AuthFailure("unauthorized")
	// The resource name is client-supplied and a peer can repeat the request
	// at will, so the line stays at V(1) and leaves the name out.
	s.Logger.V(1).Info("ECDS subscription not authorized yet", "identity", entry.identity.Key(), "uid", entry.identity.UID)
	return false
}

// spendResponse charges the stream's identity for a request the cache answers
// with the full resource: a new subscription, a request before the first
// response, or a rejection.
func (s *Server) spendResponse(entry *serverStream) error {
	key, share := "pod/"+entry.identity.PodUID, 1
	if entry.identity.PodUID == "" {
		// Pods sharing a token's ServiceAccount share its budget.
		key = "sa/" + entry.identity.Key()
		share, _, _ = s.Authz.podShare(entry.identity)
	}
	if s.responses.allow(key, share) {
		return nil
	}
	s.Metrics.Refused(RefusalResponseBudget)
	return status.Error(codes.ResourceExhausted, "too many configuration requests")
}

func (s *Server) authenticationFailure(err error) error {
	if errors.Is(err, ErrAuthRateLimited) {
		s.Metrics.AuthThrottled()
		return status.Error(codes.ResourceExhausted, "authentication rate limit exceeded")
	}
	s.Metrics.AuthFailure("error")
	return status.Error(codes.Unauthenticated, "authentication failed")
}

// pinRequestNode uses the client ID only to partition proxy retries, never to
// authorize an Engine. The client's node is reduced to the pinned ID: the
// server reads nothing else from it, and the stream keeps its first node for
// its whole lifetime.
func pinRequestNode(entry *serverStream, req *discoveryv3.DiscoveryRequest) {
	if id := req.GetNode().GetId(); id != "" && entry.proxyID == "" {
		entry.proxyID = truncateUTF8(id, maxNodeIDBytes)
	}
	if entry.proxyID == "" {
		entry.proxyID = entry.identity.Key() + "/" + entry.identity.UID
	}
	if req.Node == nil {
		req.Node = &corev3.Node{}
	} else {
		proto.Reset(req.Node)
	}
	req.Node.Id = entry.engineKey
	if entry.proxyBound {
		req.Node.Id = entry.proxyKey
	}
}

func (s *Server) ackProxyRetry(entry *serverStream, version string) {
	if entry.proxyBound && s.Store != nil {
		s.Store.AckRejected(entry.proxyKey, version)
	}
}

func (s *Server) watchNode(entry *serverStream, resourceName, engineKey, version string) string {
	proxyKey := proxyCacheKey(engineKey, entry.identity, entry.proxyID)
	if entry.proxyKey != proxyKey && entry.proxyBound && s.Store != nil {
		s.Store.ReleaseRejectedRetry(entry.proxyKey)
		entry.proxyBound = false
	}
	entry.engineKey = engineKey
	entry.proxyKey = proxyKey
	if entry.proxyBound {
		return proxyKey
	}
	if s.Store != nil {
		bound, err := s.Store.BindRejectedRetry(resourceName, proxyKey, version)
		if err != nil {
			s.Logger.Error(err, "bind proxy retry snapshot", "resource", resourceName)
		} else if bound {
			entry.proxyBound = true
			return proxyKey
		}
	}
	return engineKey
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
	if names := req.GetResourceNames(); len(names) == 1 && validResourceName(names[0]) {
		return names[0]
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
	proxyKey := s.proxyCacheKeyFor(entry, resourceName)
	if proxyKey == "" {
		return
	}
	// The retry ladder belongs to this proxy. A reconnect reuses its derived
	// version without advancing retries for other replicas.
	version := rejectedVersion
	if current, err := s.Store.CurrentVersionFor(resourceName, proxyKey); err == nil {
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

	// Envoy never re-applies a version it rejected, and the remote module fetch
	// it started needs to finish first: publish a derived version and close the
	// stream so the proxy resubscribes and applies it from the code cache.
	// Repeated NACKs before the timer fires must not schedule more timers or
	// advance the proxy's version more than once.
	entry.mu.Lock()
	if entry.retryPending || entry.closed {
		entry.mu.Unlock()
		return
	}
	entry.retryPending = true
	entry.mu.Unlock()

	// Record the budget before the timer exists: a zero RetryDelay can fire
	// immediately, and a post-close recordNack would leak a map entry.
	s.trackNack(entry, key)
	state := s.recordNack(key, base)
	s.Logger.Info("resubscribing proxy after rejected configuration",
		"resource", resourceName, "version", rejectedVersion, "attempt", attempt, "rejections", state.attempts)

	entry.mu.Lock()
	if entry.closed || !entry.retryPending {
		entry.cancelRetryLocked()
		entry.mu.Unlock()
		return
	}
	entry.retryTimer = time.AfterFunc(nackDelay(s.RetryDelay, attempt), func() {
		s.runRejectedRetry(entry, resourceName, proxyKey, base, attempt)
	})
	entry.mu.Unlock()
}

// runRejectedRetry publishes a proxy-scoped retry once the delay elapses. It
// takes ownership of the pending retry under entry.mu first, so a stream close
// or ACK can cancel it without publishing after closure.
func (s *Server) runRejectedRetry(entry *serverStream, resourceName, proxyKey, base string, attempt int) {
	entry.mu.Lock()
	if entry.closed || !entry.retryPending {
		entry.mu.Unlock()
		return
	}
	entry.retryPending = false
	entry.retryTimer = nil
	// Keep entry.mu while checking registration and bumping. onStreamClosed
	// takes the same lock before it marks the entry closed and removes it, so a
	// retry either completes while the stream is still registered or is
	// cancelled; it cannot mutate a proxy snapshot after closure.
	if _, ok := s.streams.Load(entry.streamID); !ok {
		entry.mu.Unlock()
		return
	}
	err := s.Store.BumpRejected(resourceName, proxyKey, base, attempt)
	if errors.Is(err, errSnapshotAdvanced) {
		// A newer snapshot supersedes the retry. A stream already sent it
		// keeps going; another, such as one whose first message was the
		// rejection, resubscribes to receive it.
		if current, currentErr := s.Store.CurrentVersionFor(resourceName, proxyKey); currentErr == nil && current == entry.lastVersion {
			entry.mu.Unlock()
			return
		}
		err = nil
	}
	entry.mu.Unlock()
	if err != nil {
		s.Logger.Error(err, "republish rejected configuration", "resource", resourceName)
		return
	}
	s.Streams.ResetStream(entry.session.ID)
}

// cancelRetryLocked cancels a pending rejected-configuration retry. Caller
// holds entry.mu.
func (entry *serverStream) cancelRetryLocked() {
	if entry.retryTimer != nil {
		entry.retryTimer.Stop()
		entry.retryTimer = nil
	}
	entry.retryPending = false
}

// nackDelay doubles the base delay for every retry rung of the same content,
// starting at the configured RetryDelay. A rung, not the per-stream budget,
// sets the backoff: each retry closes the stream, so the proxy's published
// version survives reconnect and records how often the content was rejected.
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
	proxyKey := s.proxyCacheKeyFor(entry, resourceName)
	if proxyKey == "" {
		return ""
	}
	version, err := s.Store.CurrentVersionFor(resourceName, proxyKey)
	if err != nil {
		return ""
	}
	return version
}

func (s *Server) onStreamClosed(streamID int64, _ *corev3.Node) {
	raw, ok := s.streams.Load(streamID)
	if !ok {
		// A refused open also triggers the closed callback; it stored no
		// entry and holds no slot.
		return
	}
	entry := raw.(*serverStream)
	entry.mu.Lock()
	if entry.closed {
		entry.mu.Unlock()
		return
	}
	entry.closed = true
	if entry.authTimer != nil {
		entry.authTimer.Stop()
	}
	entry.cancelRetryLocked()
	proxyKey, proxyBound := entry.proxyKey, entry.proxyBound
	entry.proxyBound = false
	s.streams.Delete(streamID)
	entry.mu.Unlock()
	s.clearNack(entry)
	if proxyBound && s.Store != nil {
		s.Store.ReleaseRejectedRetry(proxyKey)
	}
}

// The operator itself makes a proxy resubscribe at most once per
// rejected-configuration retry (RetryDelay, 2s at the fastest rung), and each
// retry costs a rejection plus a new subscription, so a proxy needs about one
// full response per second at most. The burst absorbs reconnect storms such as
// an operator restart or stream lifetime expiry.
const (
	responseBudgetRate  = 1 // full responses per second
	responseBudgetBurst = 16
	// responseBudgetSweep is how often full (idle) buckets are dropped.
	responseBudgetSweep = time.Minute
)

// responseBudget is a token bucket per verified identity for requests the
// cache answers with the full resource, so reopening streams or repeating
// rejections cannot force unbounded resends. A share scales the bucket for
// identities several pods use.
type responseBudget struct {
	mu        sync.Mutex
	buckets   map[string]*rate.Limiter
	lastSweep time.Time
}

func newResponseBudget() *responseBudget {
	return &responseBudget{buckets: make(map[string]*rate.Limiter)}
}

func (b *responseBudget) allow(key string, share int) bool {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Sub(b.lastSweep) >= responseBudgetSweep {
		// A full bucket carries no state, so dropping it loses nothing.
		for k, limiter := range b.buckets {
			if limiter.TokensAt(now) >= float64(limiter.Burst()) {
				delete(b.buckets, k)
			}
		}
		b.lastSweep = now
	}
	limit, burst := rate.Limit(responseBudgetRate*share), responseBudgetBurst*share
	limiter, ok := b.buckets[key]
	if !ok {
		limiter = rate.NewLimiter(limit, burst)
		b.buckets[key] = limiter
	} else {
		limiter.SetLimitAt(now, limit)
		limiter.SetBurstAt(now, burst)
	}
	return limiter.AllowN(now, 1)
}

// bearerToken extracts exactly one "Bearer <token>" authorization header.
func bearerToken(values []string) (string, error) {
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
