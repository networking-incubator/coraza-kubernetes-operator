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
	"encoding/base64"
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testToken = "test-bearer-token"

// reviewServer returns a fake TokenReview client whose result is produced by
// respond and which records every request.
func reviewServer(t *testing.T, respond func(*authv1.TokenReview) (*authv1.TokenReview, error)) (*k8sfake.Clientset, *[]authv1.TokenReviewSpec) {
	t.Helper()
	var requests []authv1.TokenReviewSpec
	client := k8sfake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		request := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		requests = append(requests, *request.Spec.DeepCopy())
		response, err := respond(request)
		if err != nil {
			return true, nil, err
		}
		return true, response, nil
	})
	return client, &requests
}

// reviewFunc is a TokenReview client that, unlike the client-go fake, serves
// reviews concurrently and sees the caller's context.
type reviewFunc func(ctx context.Context, review *authv1.TokenReview) (*authv1.TokenReview, error)

func (f reviewFunc) Create(ctx context.Context, review *authv1.TokenReview, _ metav1.CreateOptions) (*authv1.TokenReview, error) {
	return f(ctx, review)
}

// A caller that gives up during the TokenReview of a rejected token is still
// charged by the per-source failure limiter.
func TestAuthenticateChargesRejectionAfterCallerCancels(t *testing.T) {
	reviewed := make(chan struct{}, 1)
	auth := NewAuthenticator(reviewFunc(func(ctx context.Context, _ *authv1.TokenReview) (*authv1.TokenReview, error) {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		reviewed <- struct{}{}
		return rejectedReview(), nil
	}), "", time.Minute, time.Minute, time.Second, 0)
	auth.failureLimiter = newAuthFailureLimiter(0.001, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := auth.Authenticate(ctx, "rejected-1"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Authenticate() error = %v, want the review's rejection despite the cancelled caller", err)
	}
	<-reviewed
	if _, err := auth.Authenticate(context.Background(), "rejected-2"); !errors.Is(err, ErrAuthRateLimited) {
		t.Fatalf("next Authenticate() error = %v, want ErrAuthRateLimited", err)
	}
}

// A TokenReview client that panics fails the review instead of the process,
// and the failure is not charged to the peer.
func TestAuthenticateSurvivesPanickingReview(t *testing.T) {
	auth := NewAuthenticator(reviewFunc(func(context.Context, *authv1.TokenReview) (*authv1.TokenReview, error) {
		panic("client failed")
	}), "", time.Minute, time.Minute, time.Second, 0)
	auth.failureLimiter = newAuthFailureLimiter(0.001, 1)

	for _, token := range []string{"token-1", "token-2"} {
		if _, err := auth.Authenticate(context.Background(), token); err == nil || errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrAuthRateLimited) {
			t.Fatalf("Authenticate(%s) error = %v, want an uncharged review error", token, err)
		}
	}
}

// A cancelled revalidation pass, such as one running at shutdown, starts no
// TokenReview.
func TestRevalidationWithCancelledContextIssuesNoReviews(t *testing.T) {
	registry, _, resourceName, identity, _ := newRegistryFixture(t)
	var reviews atomic.Int32
	registry.auth.tokenReviews = reviewFunc(func(context.Context, *authv1.TokenReview) (*authv1.TokenReview, error) {
		reviews.Add(1)
		return rejectedReview(), nil
	})
	for i := range 3 {
		registry.Register(context.Background(), resourceName, identity, fmt.Sprintf("token-%d", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	registry.revalidateAll(ctx)
	if got := reviews.Load(); got != 0 {
		t.Fatalf("TokenReviews after cancellation = %d, want 0", got)
	}
}

// rejectedReview is how the API server answers a token it rejects.
func rejectedReview() *authv1.TokenReview {
	return &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: false, Error: "invalid bearer token"}}
}

func authenticatedReview(username string, audiences ...string) func(*authv1.TokenReview) (*authv1.TokenReview, error) {
	return func(request *authv1.TokenReview) (*authv1.TokenReview, error) {
		return &authv1.TokenReview{
			Status: authv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     audiences,
				User:          authv1.UserInfo{Username: username, UID: "uid-1"},
			},
		}, nil
	}
}

func TestAuthenticatorDefaultAudienceAcceptsEmptyStatusAudiences(t *testing.T) {
	client, requests := reviewServer(t, authenticatedReview("system:serviceaccount:shop:gateway"))
	auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Minute, time.Second, 0)

	identity, err := auth.Authenticate(context.Background(), testToken)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if identity.Namespace != "shop" || identity.Name != "gateway" || identity.UID != "uid-1" {
		t.Fatalf("identity = %+v", identity)
	}
	if len((*requests)[0].Audiences) != 0 {
		t.Fatalf("spec.audiences = %v, want omitted", (*requests)[0].Audiences)
	}
}

func TestAuthenticatorExplicitAudience(t *testing.T) {
	t.Run("matching", func(t *testing.T) {
		client, requests := reviewServer(t, authenticatedReview("system:serviceaccount:shop:gateway", "coraza-ecds"))
		auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "coraza-ecds", time.Minute, time.Minute, time.Second, 0)

		if _, err := auth.Authenticate(context.Background(), testToken); err != nil {
			t.Fatalf("Authenticate() error = %v", err)
		}
		if got := (*requests)[0].Audiences; len(got) != 1 || got[0] != "coraza-ecds" {
			t.Fatalf("spec.audiences = %v, want [coraza-ecds]", got)
		}
	})

	t.Run("non-matching", func(t *testing.T) {
		client, _ := reviewServer(t, authenticatedReview("system:serviceaccount:shop:gateway", "other"))
		auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "coraza-ecds", time.Minute, time.Minute, time.Second, 0)

		if _, err := auth.Authenticate(context.Background(), testToken); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
		}
	})

	t.Run("empty status audiences", func(t *testing.T) {
		client, _ := reviewServer(t, authenticatedReview("system:serviceaccount:shop:gateway"))
		auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "coraza-ecds", time.Minute, time.Minute, time.Second, 0)

		if _, err := auth.Authenticate(context.Background(), testToken); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
		}
	})
}

func TestAuthenticatorRejectsNonServiceAccountAndUnauthenticated(t *testing.T) {
	t.Run("user", func(t *testing.T) {
		client, _ := reviewServer(t, authenticatedReview("alice"))
		auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Minute, time.Second, 0)
		if _, err := auth.Authenticate(context.Background(), testToken); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		client, _ := reviewServer(t, func(*authv1.TokenReview) (*authv1.TokenReview, error) {
			return rejectedReview(), nil
		})
		auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Minute, time.Second, 0)
		if _, err := auth.Authenticate(context.Background(), testToken); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
		}
	})
}

func TestAuthenticatorCachesSuccessOnly(t *testing.T) {
	calls := 0
	client, _ := reviewServer(t, func(request *authv1.TokenReview) (*authv1.TokenReview, error) {
		calls++
		return authenticatedReview("system:serviceaccount:shop:gateway")(request)
	})
	auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Minute, time.Second, 0)

	if _, err := auth.Authenticate(context.Background(), testToken); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if _, err := auth.Authenticate(context.Background(), testToken); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("TokenReview calls = %d, want 1", calls)
	}
	if _, err := auth.Revalidate(context.Background(), testToken); err != nil {
		t.Fatalf("Revalidate() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("TokenReview calls after Revalidate = %d, want 2", calls)
	}

	failing := 0
	failingClient, _ := reviewServer(t, func(*authv1.TokenReview) (*authv1.TokenReview, error) {
		failing++
		return nil, errors.New("api server unavailable")
	})
	failingAuth := NewAuthenticator(failingClient.AuthenticationV1().TokenReviews(), "", time.Minute, time.Minute, time.Second, 0)
	for range 2 {
		if _, err := failingAuth.Authenticate(context.Background(), testToken); err == nil {
			t.Fatal("Authenticate() error = nil, want failure")
		}
	}
	if failing != 2 {
		t.Fatalf("TokenReview calls for failures = %d, want 2 (failures must not be cached)", failing)
	}
}

func TestAuthenticatorCacheLifetimeIsBoundedByJWTExpiry(t *testing.T) {
	auth := NewAuthenticator(nil, "", 10*time.Minute, 10*time.Minute, time.Second, 0)
	now := time.Unix(1_000_000, 0)
	auth.now = func() time.Time { return now }

	token := jwtToken(t, now.Add(10*time.Minute).Unix())
	if got, want := auth.cacheLifetime(token), 9*time.Minute; got != want {
		t.Fatalf("cacheLifetime() = %v, want %v", got, want)
	}

	expiredSoon := jwtToken(t, now.Add(30*time.Second).Unix())
	if got := auth.cacheLifetime(expiredSoon); got != 0 {
		t.Fatalf("cacheLifetime() = %v, want 0", got)
	}

	if got := auth.cacheLifetime("not-a-jwt"); got != time.Minute {
		t.Fatalf("cacheLifetime() = %v, want 1m", got)
	}
}

func jwtToken(t *testing.T, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return header + "." + payload + ".signature"
}

func TestTokenReviewTimeoutAndRetries(t *testing.T) {
	attempts := 0
	client, _ := reviewServer(t, func(*authv1.TokenReview) (*authv1.TokenReview, error) {
		attempts++
		return nil, errors.New("transient")
	})
	auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Minute, 50*time.Millisecond, 2)
	if _, err := auth.Authenticate(context.Background(), testToken); err == nil {
		t.Fatal("Authenticate() error = nil, want failure")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestAuthenticationCacheKeyDoesNotContainToken(t *testing.T) {
	key := authenticationCacheKey("secret-token", "coraza")
	if key == "secret-token" || len(key) < 64 {
		t.Fatalf("cache key %q leaks or does not hash the token", key)
	}
	other := authenticationCacheKey("secret-token", "other")
	if key == other {
		t.Fatal("cache key must include the audience")
	}
}

func TestBearerTokenRejectsOversizedToken(t *testing.T) {
	token := strings.Repeat("a", maxBearerTokenLen+1)
	if _, err := bearerToken([]string{"Bearer " + token}); err == nil {
		t.Fatal("bearerToken() error = nil, want length rejection")
	}
}

func TestAuthenticatorDoesNotCacheExpiredEntry(t *testing.T) {
	calls := 0
	client, _ := reviewServer(t, func(request *authv1.TokenReview) (*authv1.TokenReview, error) {
		calls++
		return authenticatedReview("system:serviceaccount:shop:gateway")(request)
	})
	auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Minute, time.Second, 0)
	now := time.Unix(1_000_000, 0)
	auth.now = func() time.Time { return now }

	if _, err := auth.Authenticate(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := auth.Authenticate(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("TokenReview calls = %d, want 2 after cache expiry", calls)
	}
}

func newRegistryFixture(t *testing.T) (*StreamRegistry, *AuthzIndex, string, Identity, func(bool)) {
	t.Helper()

	identity := Identity{Namespace: testNamespace, Name: "gateway", UID: "sa-uid"}
	resourceName := ResourceName(testNamespace, "checkout", "engine-uid")

	var mu sync.Mutex
	valid := true
	fakeClient := k8sfake.NewSimpleClientset()
	fakeClient.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		ok := valid
		mu.Unlock()
		return true, reviewFor(identity, validToken, ok), nil
	})

	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}

	authz := NewAuthzIndex(nil, nil, time.Hour, nil)
	authz.entries[resourceName] = AuthzEntry{
		ResourceName: resourceName,
		Namespace:    testNamespace,
		EngineName:   "checkout",
		Identities:   map[string]string{identity.Key(): identity.UID},
		RefreshedAt:  time.Now(),
	}

	auth := NewAuthenticator(fakeClient.AuthenticationV1().TokenReviews(), "", time.Minute, time.Hour, time.Second, 0)
	registry := NewStreamRegistry(auth, authz, DefaultLimits(), metrics)
	setValid := func(value bool) {
		mu.Lock()
		valid = value
		mu.Unlock()
	}
	return registry, authz, resourceName, identity, setValid
}

// A stream closes only after two consecutive revalidation rejections of its
// token: the API server reports its own authenticator failures in the same
// shape, so a single rejection followed by a success keeps the stream.
func TestStreamRegistryRevalidationFailuresCloseStream(t *testing.T) {
	registry, _, resourceName, identity, setValid := newRegistryFixture(t)
	ctx := context.Background()
	if _, err := registry.auth.Authenticate(ctx, validToken); err != nil {
		t.Fatal(err)
	}
	session := registry.Register(ctx, resourceName, identity, validToken)

	setValid(false)
	registry.revalidateAll(ctx)
	if cause := context.Cause(session.Context()); cause != nil {
		t.Fatalf("context cause after one rejection = %v, want the stream kept", cause)
	}
	if _, ok := registry.auth.cache.Load(authenticationCacheKey(validToken, "")); ok {
		t.Fatal("a rejected token must be purged from the success cache")
	}

	setValid(true)
	registry.revalidateAll(ctx)
	setValid(false)
	registry.revalidateAll(ctx)
	if cause := context.Cause(session.Context()); cause != nil {
		t.Fatalf("context cause = %v, want a success to reset the rejection count", cause)
	}

	registry.revalidateAll(ctx)
	if cause := context.Cause(session.Context()); !errors.Is(cause, ErrRevalidateFailed) {
		t.Fatalf("context cause = %v, want ErrRevalidateFailed after two consecutive rejections", cause)
	}
}

func TestAuthenticatorCarriesPodUID(t *testing.T) {
	client, _ := reviewServer(t, func(*authv1.TokenReview) (*authv1.TokenReview, error) {
		return &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true,
			User: authv1.UserInfo{
				Username: "system:serviceaccount:shop:gateway",
				UID:      "sa-uid",
				Extra:    map[string]authv1.ExtraValue{podUIDExtraKey: {"pod-uid-1"}},
			},
		}}, nil
	})
	auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Minute, time.Second, 0)

	for range 2 { // the second call is served from the success cache
		identity, err := auth.Authenticate(context.Background(), testToken)
		if err != nil {
			t.Fatal(err)
		}
		if identity.PodUID != "pod-uid-1" {
			t.Fatalf("PodUID = %q, want pod-uid-1", identity.PodUID)
		}
	}
}

// TestStreamRegistryKeepsStreamOnTransientRevalidationError covers an API-server
// outage: a token that cannot be reviewed is not a rejected token, so the
// stream stays open until the next tick or its lifetime bound.
func TestStreamRegistryKeepsStreamOnTransientRevalidationError(t *testing.T) {
	registry, _, resourceName, identity, _ := newRegistryFixture(t)
	session := registry.Register(context.Background(), resourceName, identity, validToken)

	unavailable := k8sfake.NewSimpleClientset()
	unavailable.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	registry.auth = NewAuthenticator(unavailable.AuthenticationV1().TokenReviews(), "", time.Minute, time.Hour, time.Second, 0)

	registry.revalidateAll(context.Background())

	if cause := context.Cause(session.Context()); cause != nil {
		t.Fatalf("context cause = %v, want the stream kept on a transient error", cause)
	}
}

func TestStreamRegistryRevokesWhenAuthorizationIsGone(t *testing.T) {
	registry, authz, resourceName, identity, _ := newRegistryFixture(t)
	session := registry.Register(context.Background(), resourceName, identity, validToken)

	authz.mu.Lock()
	delete(authz.entries, resourceName)
	authz.mu.Unlock()
	registry.revalidateAll(context.Background())

	if cause := context.Cause(session.Context()); !errors.Is(cause, ErrRevoked) {
		t.Fatalf("context cause = %v, want ErrRevoked", cause)
	}
}

func TestStreamRegistryExpiresStreamLifetime(t *testing.T) {
	registry, _, resourceName, identity, _ := newRegistryFixture(t)
	session := registry.Register(context.Background(), resourceName, identity, validToken)

	registry.mu.Lock()
	session.expiresAt = time.Now().Add(-time.Second)
	registry.mu.Unlock()
	registry.revalidateAll(context.Background())

	if cause := context.Cause(session.Context()); !errors.Is(cause, ErrLifetimeExpired) {
		t.Fatalf("context cause = %v, want ErrLifetimeExpired", cause)
	}
}

func TestStreamJitterIsBounded(t *testing.T) {
	lifetime := 10 * time.Minute
	for range 100 {
		jitter := streamJitter(lifetime)
		if jitter < 0 || jitter > lifetime/10 {
			t.Fatalf("streamJitter() = %v, want within [0, %v]", jitter, lifetime/10)
		}
	}
	if streamJitter(time.Nanosecond) != 0 {
		t.Fatal("streamJitter() must be zero for very short lifetimes")
	}
}

func TestRevalidationReviewsEachDistinctTokenOnce(t *testing.T) {
	client, requests := reviewServer(t, authenticatedReview("system:serviceaccount:default:gateway"))
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	resourceName := ResourceName(testNamespace, "checkout", "engine-uid")
	identity := Identity{Namespace: testNamespace, Name: "gateway", UID: "uid-1"}
	authz := NewAuthzIndex(nil, nil, time.Hour, nil)
	authz.entries[resourceName] = AuthzEntry{
		ResourceName: resourceName,
		Namespace:    testNamespace,
		EngineName:   "checkout",
		Identities:   map[string]string{identity.Key(): identity.UID},
		RefreshedAt:  time.Now(),
	}
	auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Hour, time.Second, 0)
	registry := NewStreamRegistry(auth, authz, DefaultLimits(), metrics)

	for range 3 {
		registry.Register(context.Background(), resourceName, identity, testToken)
	}
	registry.revalidateAll(context.Background())

	if got := len(*requests); got != 1 {
		t.Fatalf("TokenReview calls = %d, want 1 for a shared token", got)
	}
}

// blockingReviewAuth returns an authenticator whose TokenReviews block until
// release closes and an in-flight counter for the calls currently parked there.
func blockingReviewAuth(t *testing.T) (*Authenticator, *atomic.Int32, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	var inFlight atomic.Int32
	respond := authenticatedReview("system:serviceaccount:shop:gateway")
	client, _ := reviewServer(t, func(review *authv1.TokenReview) (*authv1.TokenReview, error) {
		inFlight.Add(1)
		<-release
		return respond(review)
	})
	auth := NewAuthenticator(client.AuthenticationV1().TokenReviews(), "", time.Minute, time.Hour, time.Second, 0)
	return auth, &inFlight, release
}

func TestReviewTokensBoundsGoroutines(t *testing.T) {
	auth, inFlight, release := blockingReviewAuth(t)
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	registry := NewStreamRegistry(auth, NewAuthzIndex(nil, nil, time.Hour, nil), DefaultLimits(), metrics)

	tokens := make([]string, 8*revalidationConcurrency)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("token-%d", i)
	}

	before := goruntime.NumGoroutine()
	done := make(chan struct{})
	go func() {
		registry.reviewTokens(context.Background(), tokens)
		close(done)
	}()

	// The fake clientset serializes TokenReviews, so one parked review implies
	// the whole pool was spawned and the feeder is out of slots: count
	// goroutines now; the pool must size them, not just the concurrent calls.
	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("TokenReviews in flight = %d, want at least 1", inFlight.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if peak := goruntime.NumGoroutine() - before; peak > 4*revalidationConcurrency {
		t.Fatalf("goroutines during review = %d, want at most about %d", peak, revalidationConcurrency)
	}

	close(release)
	<-done
}

func TestRevalidationRefreshesExpiryClockAfterSlowPass(t *testing.T) {
	auth, inFlight, release := blockingReviewAuth(t)
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	resourceName := ResourceName(testNamespace, "checkout", "engine-uid")
	identity := Identity{Namespace: testNamespace, Name: "gateway", UID: "uid-1"}
	authz := NewAuthzIndex(nil, nil, time.Hour, nil)
	authz.entries[resourceName] = AuthzEntry{
		ResourceName: resourceName,
		Namespace:    testNamespace,
		EngineName:   "checkout",
		Identities:   map[string]string{identity.Key(): identity.UID},
		RefreshedAt:  time.Now(),
	}
	registry := NewStreamRegistry(auth, authz, DefaultLimits(), metrics)

	session := registry.Register(context.Background(), resourceName, identity, testToken)
	// The stream expires while the pass is parked in the TokenReview: it is
	// live when the pass snapshots the targets, expired by the time the
	// reviews return, so only a post-pass clock check can see it.
	registry.mu.Lock()
	session.expiresAt = time.Now().Add(300 * time.Millisecond)
	registry.mu.Unlock()

	done := make(chan struct{})
	go func() {
		registry.revalidateAll(context.Background())
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("revalidation pass never reached the TokenReview")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	close(release)
	<-done

	if cause := context.Cause(session.Context()); !errors.Is(cause, ErrLifetimeExpired) {
		t.Fatalf("context cause = %v, want ErrLifetimeExpired", cause)
	}
}
