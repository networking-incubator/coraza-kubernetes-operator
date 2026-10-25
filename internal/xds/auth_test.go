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
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
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
			return &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: false}}, nil
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
