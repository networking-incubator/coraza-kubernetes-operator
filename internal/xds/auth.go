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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

const (
	serviceAccountUsernamePrefix = "system:serviceaccount:"
	serviceAccountUsernameParts  = 2
	bearerScheme                 = "bearer"
	// maxBearerTokenLen bounds the token before it is hashed and reviewed. It is
	// far above any ServiceAccount JWT.
	maxBearerTokenLen = 64 << 10
)

// ErrUnauthenticated marks a rejected token. Callers map it to an
// Unauthenticated gRPC status and never leak the underlying detail.
var ErrUnauthenticated = errors.New("token is not authenticated")

// Identity is the authenticated ServiceAccount behind a stream.
type Identity struct {
	Namespace string
	Name      string
	UID       string
}

// Key returns the "namespace/name" authorization key.
func (i Identity) Key() string {
	return namespacedNameKey(i.Namespace, i.Name)
}

type cachedIdentity struct {
	identity  Identity
	expiresAt time.Time
}

// Authenticator validates bearer tokens with TokenReview and caches successful
// results only. Tokens are never logged and never used as metric labels.
type Authenticator struct {
	tokenReviews   authclient.TokenReviewInterface
	audience       string
	cacheTTL       time.Duration
	streamLifetime time.Duration
	timeout        time.Duration
	retries        int
	now            func() time.Time
	cache          sync.Map // sha256(token)+"|"+audience -> cachedIdentity
}

// NewAuthenticator returns an Authenticator for the given client. An empty
// audience means the API server's default audience is used.
func NewAuthenticator(client authclient.TokenReviewInterface, audience string, cacheTTL, streamLifetime, timeout time.Duration, retries int) *Authenticator {
	if cacheTTL <= 0 {
		cacheTTL = DefaultTokenReviewRevalidate
	}
	if streamLifetime <= 0 {
		streamLifetime = DefaultStreamMaxLifetime
	}
	if timeout <= 0 {
		timeout = DefaultTokenReviewTimeout
	}
	if retries < 0 {
		retries = 0
	}
	return &Authenticator{
		tokenReviews:   client,
		audience:       audience,
		cacheTTL:       cacheTTL,
		streamLifetime: streamLifetime,
		timeout:        timeout,
		retries:        retries,
		now:            time.Now,
	}
}

// Authenticate validates token, reusing a successful cached result when it is
// still fresh. Failures are never cached.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (*Identity, error) {
	key := authenticationCacheKey(token, a.audience)
	if cached, ok := a.cache.Load(key); ok {
		entry := cached.(cachedIdentity)
		if a.now().Before(entry.expiresAt) {
			identity := entry.identity
			return &identity, nil
		}
		a.cache.Delete(key)
	}

	identity, err := a.review(ctx, token)
	if err != nil {
		return nil, err
	}

	a.storeCachedIdentity(key, token, identity)
	return identity, nil
}

// storeCachedIdentity stores a successful review and opportunistically drops
// expired entries so rotated tokens do not accumulate for the process lifetime.
func (a *Authenticator) storeCachedIdentity(key, token string, identity *Identity) {
	if ttl := a.cacheLifetime(token); ttl > 0 {
		a.sweepExpired()
		a.cache.Store(key, cachedIdentity{identity: *identity, expiresAt: a.now().Add(ttl)})
	}
}

// sweepExpired removes cache entries whose lifetime has ended.
func (a *Authenticator) sweepExpired() {
	now := a.now()
	a.cache.Range(func(key, value any) bool {
		if !now.Before(value.(cachedIdentity).expiresAt) {
			a.cache.Delete(key)
		}
		return true
	})
}

// Revalidate re-runs TokenReview without reading the cache. A successful
// result refreshes the cache entry.
func (a *Authenticator) Revalidate(ctx context.Context, token string) (*Identity, error) {
	identity, err := a.review(ctx, token)
	if err != nil {
		return nil, err
	}
	a.storeCachedIdentity(authenticationCacheKey(token, a.audience), token, identity)
	return identity, nil
}

// cacheLifetime returns min(configured, JWT exp-1m, stream lifetime). An
// unparseable exp falls back to one minute. exp bounds the cache only; it is
// never used for authorization.
func (a *Authenticator) cacheLifetime(token string) time.Duration {
	ttl := a.cacheTTL
	if a.streamLifetime > 0 && a.streamLifetime < ttl {
		ttl = a.streamLifetime
	}
	if expiry, ok := jwtExpiry(token); ok {
		remaining := expiry.Sub(a.now()) - time.Minute
		if remaining <= 0 {
			return 0
		}
		if remaining < ttl {
			ttl = remaining
		}
	} else if ttl > time.Minute {
		ttl = time.Minute
	}
	return ttl
}

func (a *Authenticator) review(ctx context.Context, token string) (*Identity, error) {
	request := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{Token: token},
	}
	if a.audience != "" {
		request.Spec.Audiences = []string{a.audience}
	}

	var lastErr error
	for attempt := 0; attempt <= a.retries; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, a.timeout)
		review, err := a.tokenReviews.Create(callCtx, request, metav1.CreateOptions{})
		cancel()
		if err == nil {
			return a.identityFromReview(review)
		}
		lastErr = err

		if attempt == a.retries {
			break
		}
		backoff := time.Duration(100*(1<<attempt)) * time.Millisecond
		if backoff > time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}

	return nil, fmt.Errorf("token review: %w", lastErr)
}

func (a *Authenticator) identityFromReview(review *authv1.TokenReview) (*Identity, error) {
	if review == nil || !review.Status.Authenticated {
		return nil, ErrUnauthenticated
	}

	if a.audience == "" {
		// The API server validates the default audience. Only this mode accepts
		// an empty status.audiences list.
	} else if !slices.Contains(review.Status.Audiences, a.audience) {
		return nil, fmt.Errorf("%w: token audience does not match", ErrUnauthenticated)
	}

	rest, ok := strings.CutPrefix(review.Status.User.Username, serviceAccountUsernamePrefix)
	if !ok {
		return nil, fmt.Errorf("%w: username is not a ServiceAccount", ErrUnauthenticated)
	}
	parts := strings.SplitN(rest, ":", serviceAccountUsernameParts)
	if len(parts) != serviceAccountUsernameParts || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("%w: malformed ServiceAccount username", ErrUnauthenticated)
	}

	return &Identity{
		Namespace: parts[0],
		Name:      parts[1],
		UID:       review.Status.User.UID,
	}, nil
}

func authenticationCacheKey(token, audience string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]) + "|" + audience
}

// jwtExpiry extracts exp from an unverified JWT payload. It only bounds cache
// lifetime; authorization always comes from TokenReview.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
