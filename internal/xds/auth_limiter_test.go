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
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/peer"
)

// handlerAddr mimics the string-backed "host:port" address grpc-go's
// ServeHTTP transport puts in the peer context, which the operator's muxed
// listener uses in production.
type handlerAddr string

func (h handlerAddr) Network() string { return "http" }
func (h handlerAddr) String() string  { return string(h) }

// Without the string-address fallback every production peer maps to
// "unknown", so one attacker's failed tokens throttle the whole fleet.
func TestAuthFailurePeerAddressParsesServeHTTPPeer(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: handlerAddr("10.1.2.3:51234")})
	if got := authFailurePeerAddress(ctx); got != "10.1.2.3" {
		t.Fatalf("authFailurePeerAddress() = %q, want 10.1.2.3", got)
	}

	// Distinct peers must land in distinct buckets, not the shared fallback.
	limiter := newAuthFailureLimiter(1000, 1)
	first := peer.NewContext(context.Background(), &peer.Peer{Addr: handlerAddr("10.1.2.3:1")})
	second := peer.NewContext(context.Background(), &peer.Peer{Addr: handlerAddr("10.1.2.4:1")})
	if _, allowed := limiter.reserve(first); !allowed {
		t.Fatal("first peer must be allowed")
	}
	if _, allowed := limiter.reserve(second); !allowed {
		t.Fatal("a second peer must not share the first peer's bucket")
	}
	if _, allowed := limiter.reserve(first); allowed {
		t.Fatal("the first peer's bucket must be exhausted")
	}
}

// A peer spray from many source IPs must not grow the bucket map without
// limit: beyond the cap every new peer shares the overflow bucket.
func TestAuthFailureLimiterBoundsBucketCardinality(t *testing.T) {
	limiter := newAuthFailureLimiter(1000, 1)
	for i := 0; i < maxAuthFailureBuckets; i++ {
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: handlerAddr(fmt.Sprintf("10.0.%d.%d:1", i/256, i%256))})
		if _, allowed := limiter.reserve(ctx); !allowed {
			t.Fatalf("peer %d must be allowed", i)
		}
	}
	limiter.mu.Lock()
	size := len(limiter.buckets)
	limiter.mu.Unlock()
	if size != maxAuthFailureBuckets {
		t.Fatalf("bucket count = %d, want %d", size, maxAuthFailureBuckets)
	}

	overflow := peer.NewContext(context.Background(), &peer.Peer{Addr: handlerAddr("192.0.2.99:1")})
	reservation, allowed := limiter.reserve(overflow)
	if !allowed {
		t.Fatal("the overflow peer must draw from the shared bucket, not be rejected outright")
	}
	if reservation.bucket != limiter.buckets[unknownAuthPeer] {
		t.Fatal("a peer beyond the cap must share the unknown overflow bucket")
	}
	reservation.finish(true)
	// The overflow bucket may itself be created past the cap (a single extra
	// entry), but further new peers must reuse it rather than grow the map.
	second := peer.NewContext(context.Background(), &peer.Peer{Addr: handlerAddr("192.0.2.100:1")})
	secondReservation, allowed := limiter.reserve(second)
	if !allowed {
		t.Fatal("a second overflow peer must be allowed")
	}
	if secondReservation.bucket != reservation.bucket {
		t.Fatal("overflow peers must share one bucket")
	}
	limiter.mu.Lock()
	size = len(limiter.buckets)
	limiter.mu.Unlock()
	if size > maxAuthFailureBuckets+1 {
		t.Fatalf("bucket count after overflow = %d, want at most %d", size, maxAuthFailureBuckets+1)
	}
}

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
