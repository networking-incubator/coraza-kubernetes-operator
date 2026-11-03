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
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/peer"
)

func TestAuthFailureLimiterEvictsIdleBuckets(t *testing.T) {
	limiter := newAuthFailureLimiter(1, 1)
	now := time.Unix(1_000_000, 0)
	limiter.now = func() time.Time { return now }

	firstPeer := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1")},
	})
	reservation, ok := limiter.reserve(firstPeer)
	if !ok {
		t.Fatal("first peer did not receive its initial token")
	}
	reservation.finish(false)

	now = now.Add(authFailureBucketIdleTTL + authFailureBucketSweepPeriod)
	secondPeer := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.2")},
	})
	if _, ok := limiter.reserve(secondPeer); !ok {
		t.Fatal("second peer did not receive its initial token")
	}

	if got := len(limiter.buckets); got != 1 {
		t.Fatalf("peer buckets = %d, want only the active peer after idle sweep", got)
	}
}

func TestAuthFailureLimiterUsesBoundedUnknownPeer(t *testing.T) {
	if got := authFailurePeerAddress(context.Background()); got != unknownAuthPeer {
		t.Fatalf("authFailurePeerAddress() = %q, want %q", got, unknownAuthPeer)
	}
	if got := authFailurePeerAddress(peer.NewContext(context.Background(), nil)); got != unknownAuthPeer {
		t.Fatalf("authFailurePeerAddress(nil peer) = %q, want %q", got, unknownAuthPeer)
	}
}

func TestAuthFailureLimiterBoundsConcurrentReservations(t *testing.T) {
	const burst, attempts = 4, 20
	limiter := newAuthFailureLimiter(1, burst)
	limiter.now = func() time.Time { return time.Unix(1_000_000, 0) }
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1")},
	})

	start := make(chan struct{})
	reservations := make(chan *authFailureReservation, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if reservation, ok := limiter.reserve(ctx); ok {
				reservations <- reservation
			}
		}()
	}
	close(start)
	wg.Wait()
	close(reservations)

	allowed := 0
	for reservation := range reservations {
		allowed++
		reservation.finish(false)
	}
	if allowed != burst {
		t.Fatalf("concurrent reservations = %d, want burst %d", allowed, burst)
	}
	if _, ok := limiter.reserve(ctx); ok {
		t.Fatal("limiter allowed an extra attempt after the concurrent failures")
	}
}
