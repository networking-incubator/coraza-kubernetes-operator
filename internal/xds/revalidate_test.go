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
	"sync"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/prometheus/client_golang/prometheus"
)

func newRegistryFixture(t *testing.T, limits Limits) (*StreamRegistry, *AuthzIndex, string, Identity, func(bool)) {
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
		if !ok {
			return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: false}}, nil
		}
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true,
			User:          authv1.UserInfo{Username: "system:serviceaccount:" + identity.Namespace + ":" + identity.Name, UID: identity.UID},
		}}, nil
	})

	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}

	authz := NewAuthzIndex(nil, time.Hour)
	authz.entries[resourceName] = AuthzEntry{
		ResourceName: resourceName,
		Namespace:    testNamespace,
		EngineName:   "checkout",
		Identities:   map[string]string{identity.Key(): identity.UID},
		RefreshedAt:  time.Now(),
	}

	auth := NewAuthenticator(fakeClient.AuthenticationV1().TokenReviews(), "", time.Minute, time.Hour, time.Second, 0)
	registry := NewStreamRegistry(auth, authz, limits, metrics)
	setValid := func(value bool) {
		mu.Lock()
		valid = value
		mu.Unlock()
	}
	return registry, authz, resourceName, identity, setValid
}

func TestStreamRegistryEnforcesCaps(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStreams = 2
	limits.MaxStreamsPerIdentity = 1
	registry, _, resourceName, identity, _ := newRegistryFixture(t, limits)
	ctx := context.Background()

	if _, err := registry.Register(ctx, resourceName, identity, validToken); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	_, err := registry.Register(ctx, resourceName, identity, validToken)
	var limitErr *StreamLimitError
	if !errors.As(err, &limitErr) || !limitErr.Identity {
		t.Fatalf("Register() error = %v, want identity limit", err)
	}

	other := Identity{Namespace: testNamespace, Name: "other", UID: "sa-2"}
	if _, err := registry.Register(ctx, resourceName, other, validToken); err != nil {
		t.Fatalf("Register(other identity) error = %v", err)
	}
	third := Identity{Namespace: testNamespace, Name: "third", UID: "sa-3"}
	if _, err := registry.Register(ctx, resourceName, third, validToken); err == nil {
		t.Fatal("Register() error = nil, want global stream limit")
	}
}

func TestStreamRegistryRevalidationFailuresCloseStream(t *testing.T) {
	registry, _, resourceName, identity, setValid := newRegistryFixture(t, DefaultLimits())
	session, err := registry.Register(context.Background(), resourceName, identity, validToken)
	if err != nil {
		t.Fatal(err)
	}

	setValid(false)
	registry.revalidateAll(context.Background())

	if cause := context.Cause(session.Context()); !errors.Is(cause, ErrRevalidateFailed) {
		t.Fatalf("context cause = %v, want ErrRevalidateFailed", cause)
	}
}

func TestStreamRegistryRevokesWhenAuthorizationIsGone(t *testing.T) {
	registry, authz, resourceName, identity, _ := newRegistryFixture(t, DefaultLimits())
	session, err := registry.Register(context.Background(), resourceName, identity, validToken)
	if err != nil {
		t.Fatal(err)
	}

	authz.mu.Lock()
	delete(authz.entries, resourceName)
	authz.mu.Unlock()
	registry.revalidateAll(context.Background())

	if cause := context.Cause(session.Context()); !errors.Is(cause, ErrRevoked) {
		t.Fatalf("context cause = %v, want ErrRevoked", cause)
	}
}

func TestStreamRegistryExpiresStreamLifetime(t *testing.T) {
	registry, _, resourceName, identity, _ := newRegistryFixture(t, DefaultLimits())
	session, err := registry.Register(context.Background(), resourceName, identity, validToken)
	if err != nil {
		t.Fatal(err)
	}

	registry.mu.Lock()
	session.expiresAt = time.Now().Add(-time.Second)
	registry.mu.Unlock()
	registry.revalidateAll(context.Background())

	if cause := context.Cause(session.Context()); !errors.Is(cause, ErrLifetimeExpired) {
		t.Fatalf("context cause = %v, want ErrLifetimeExpired", cause)
	}
}

func TestStreamRegistryUnregisterReleasesSlots(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStreams = 1
	limits.MaxStreamsPerIdentity = 8
	registry, _, resourceName, identity, _ := newRegistryFixture(t, limits)

	session, err := registry.Register(context.Background(), resourceName, identity, validToken)
	if err != nil {
		t.Fatal(err)
	}
	registry.Unregister(session.ID)

	if _, err := registry.Register(context.Background(), resourceName, identity, validToken); err != nil {
		t.Fatalf("Register() after Unregister error = %v", err)
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
