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
	"math/rand/v2"
	"sync"
	"time"
)

// Stream close causes. The server maps them to bounded gRPC statuses.
var (
	// ErrResubscribe closes a stream so its client resubscribes, for example
	// after a rejected configuration that needs the remote module first.
	ErrResubscribe = errors.New("resubscribe required")

	// ErrRevoked means the Engine or Gateway mapping no longer authorizes the
	// stream.
	ErrRevoked = errors.New("authorization revoked")
	// ErrRevalidateFailed means the stream token failed revalidation.
	ErrRevalidateFailed = errors.New("stream revalidation failed")
	// ErrLifetimeExpired forces a reconnect so a rotated token is re-read.
	ErrLifetimeExpired = errors.New("stream lifetime expired")
	// ErrServerShutdown closes streams during shutdown.
	ErrServerShutdown = errors.New("server shutting down")
)

// StreamLimitError is returned when a stream cap is exceeded.
type StreamLimitError struct {
	Identity bool
}

func (e *StreamLimitError) Error() string {
	if e.Identity {
		return "too many streams for this identity"
	}
	return "too many streams"
}

// StreamSession is one registered ECDS stream.
type StreamSession struct {
	ID           uint64
	ResourceName string
	Identity     Identity
	token        string
	StartedAt    time.Time
	expiresAt    time.Time

	ctx    context.Context
	cancel context.CancelCauseFunc
}

// Context returns the session context. It is cancelled with a bounded cause
// when the stream must end.
func (s *StreamSession) Context() context.Context {
	return s.ctx
}

// StreamRegistry holds active streams, enforces caps, and revalidates them.
type StreamRegistry struct {
	auth    *Authenticator
	authz   *AuthzIndex
	limits  Limits
	metrics *Metrics
	now     func() time.Time

	mu          sync.Mutex
	nextID      uint64
	streams     map[uint64]*StreamSession
	perIdentity map[string]int
}

// NewStreamRegistry returns an empty registry.
func NewStreamRegistry(auth *Authenticator, authz *AuthzIndex, limits Limits, metrics *Metrics) *StreamRegistry {
	return &StreamRegistry{
		auth:        auth,
		authz:       authz,
		limits:      limits.WithDefaults(),
		metrics:     metrics,
		now:         time.Now,
		streams:     make(map[uint64]*StreamSession),
		perIdentity: make(map[string]int),
	}
}

// Register reserves a stream slot and returns its session.
func (r *StreamRegistry) Register(parent context.Context, resourceName string, id Identity, token string) (*StreamSession, error) {
	return r.RegisterWithCancel(parent, nil, resourceName, id, token)
}

// RegisterWithCancel reserves a stream slot using cancel as the stream's
// lifecycle cancel. A nil cancel creates a child context owned by the registry.
func (r *StreamRegistry) RegisterWithCancel(parent context.Context, cancel context.CancelCauseFunc, resourceName string, id Identity, token string) (*StreamSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.streams) >= r.limits.MaxStreams {
		return nil, &StreamLimitError{}
	}
	if r.perIdentity[id.Key()] >= r.limits.MaxStreamsPerIdentity {
		return nil, &StreamLimitError{Identity: true}
	}

	now := r.now()
	ctx := parent
	if cancel == nil {
		ctx, cancel = context.WithCancelCause(parent)
	}
	r.nextID++
	session := &StreamSession{
		ID:           r.nextID,
		ResourceName: resourceName,
		Identity:     id,
		token:        token,
		StartedAt:    now,
		expiresAt:    now.Add(r.limits.StreamMaxLifetime + streamJitter(r.limits.StreamMaxLifetime)),
		ctx:          ctx,
		cancel:       cancel,
	}
	r.streams[session.ID] = session
	r.perIdentity[id.Key()]++
	r.metrics.StreamOpened()
	return session, nil
}

// Unregister releases a stream slot.
func (r *StreamRegistry) Unregister(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.streams[id]
	if !ok {
		return
	}
	delete(r.streams, id)
	if count := r.perIdentity[session.Identity.Key()]; count > 1 {
		r.perIdentity[session.Identity.Key()] = count - 1
	} else {
		delete(r.perIdentity, session.Identity.Key())
	}
	r.metrics.StreamClosed()
}

// ResetStream closes one live stream with a retryable status so the client
// resubscribes.
func (r *StreamRegistry) ResetStream(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session, ok := r.streams[id]; ok {
		session.cancel(ErrResubscribe)
	}
}

// SetResource changes the resource a live stream subscribes to after a
// re-authorized subscription update.
func (r *StreamRegistry) SetResource(id uint64, resourceName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session, ok := r.streams[id]; ok {
		session.ResourceName = resourceName
	}
}

// RevokeResource closes every stream for one resource.
func (r *StreamRegistry) RevokeResource(resourceName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, session := range r.streams {
		if session.ResourceName == resourceName {
			session.cancel(ErrRevoked)
		}
	}
}

// CloseAll closes every stream during shutdown.
func (r *StreamRegistry) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, session := range r.streams {
		session.cancel(ErrServerShutdown)
	}
}

// Start runs the revalidation loop until ctx ends. It implements
// manager.Runnable.
func (r *StreamRegistry) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.limits.TokenReviewRevalidate)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.CloseAll()
			return nil
		case <-ticker.C:
			r.revalidateAll(ctx)
		}
	}
}

// NeedLeaderElection keeps revalidation running on every replica.
func (r *StreamRegistry) NeedLeaderElection() bool {
	return false
}

func (r *StreamRegistry) revalidateAll(ctx context.Context) {
	// Refresh the mapping first so revalidation authorizes against current
	// Engine/Gateway/Pod/ServiceAccount state.
	r.authz.RefreshAll(ctx)

	r.mu.Lock()
	targets := make([]revalidationTarget, 0, len(r.streams))
	for _, session := range r.streams {
		targets = append(targets, revalidationTarget{
			session:      session,
			resourceName: session.ResourceName,
			identity:     session.Identity,
			token:        session.token,
			expiresAt:    session.expiresAt,
		})
	}
	r.mu.Unlock()

	now := r.now()
	tokens := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if now.After(target.expiresAt) {
			continue
		}
		if _, ok := seen[target.token]; ok {
			continue
		}
		seen[target.token] = struct{}{}
		tokens = append(tokens, target.token)
	}
	reviews := r.reviewTokens(ctx, tokens)

	// The review pass can span several TokenReview timeouts; re-check expiry
	// against a fresh clock so near-expiry streams are not carried an extra
	// tick.
	now = r.now()
	for _, target := range targets {
		if now.After(target.expiresAt) {
			target.session.cancel(ErrLifetimeExpired)
			continue
		}

		review, ok := reviews[target.token]
		if !ok {
			r.metrics.AuthFailure("error")
			target.session.cancel(ErrRevalidateFailed)
			continue
		}
		if review.err != nil {
			r.metrics.AuthFailure("error")
			target.session.cancel(ErrRevalidateFailed)
			continue
		}
		if review.identity.UID != "" && target.identity.UID != "" && review.identity.UID != target.identity.UID {
			r.metrics.AuthFailure("unauthorized")
			target.session.cancel(ErrRevoked)
			continue
		}
		if !r.authz.Authorize(target.resourceName, *review.identity) {
			r.metrics.AuthFailure("unauthorized")
			target.session.cancel(ErrRevoked)
		}
	}
}

// revalidationConcurrency bounds simultaneous TokenReviews in one pass.
const revalidationConcurrency = 8

type tokenReviewResult struct {
	identity *Identity
	err      error
}

// reviewTokens reviews every distinct token with a bounded worker pool. Streams
// that share a token share one TokenReview result. The pool sizes the
// goroutine count itself, so a pass cannot park one goroutine per token.
func (r *StreamRegistry) reviewTokens(ctx context.Context, tokens []string) map[string]tokenReviewResult {
	results := make([]tokenReviewResult, len(tokens))
	workers := min(len(tokens), revalidationConcurrency)
	if workers == 0 {
		return make(map[string]tokenReviewResult)
	}
	queue := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				identity, err := r.auth.Revalidate(ctx, tokens[i])
				results[i] = tokenReviewResult{identity: identity, err: err}
			}
		}()
	}
	for i := range tokens {
		queue <- i
	}
	close(queue)
	wg.Wait()

	reviews := make(map[string]tokenReviewResult, len(tokens))
	for i, token := range tokens {
		reviews[token] = results[i]
	}
	return reviews
}

type revalidationTarget struct {
	session      *StreamSession
	resourceName string
	identity     Identity
	token        string
	expiresAt    time.Time
}

// streamJitter spreads forced reconnects over up to 10% of the lifetime.
func streamJitter(lifetime time.Duration) time.Duration {
	window := int64(lifetime / 10)
	if window <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(window + 1)) //nolint:gosec // reconnect jitter, not a security boundary
}
