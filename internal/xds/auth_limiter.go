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
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/grpc/peer"
)

const (
	unknownAuthPeer              = "unknown"
	authFailureBucketIdleTTL     = 10 * time.Minute
	authFailureBucketSweepPeriod = time.Minute
	// maxAuthFailureBuckets bounds the peer-bucket map. An attacker spraying
	// failed tokens from many source IPs cannot grow it without limit; beyond
	// the cap every new peer shares the "unknown" overflow bucket, which is
	// itself rate-limited.
	maxAuthFailureBuckets = 4096
)

type authFailureBucket struct {
	limiter  *rate.Limiter
	pending  int // provisional permits held until TokenReview returns
	lastSeen time.Time
}

type authFailureReservation struct {
	limiter *authFailureLimiter
	bucket  *authFailureBucket
	once    sync.Once
}

func (r *authFailureReservation) finish(success bool) {
	r.once.Do(func() {
		l := r.limiter
		now := l.now()
		l.mu.Lock()
		defer l.mu.Unlock()
		if !success {
			// The pending permit guarantees this token is still available.
			r.bucket.limiter.AllowN(now, 1)
		}
		r.bucket.pending--
		r.bucket.lastSeen = now
	})
}

// authFailureLimiter holds provisional capacity during TokenReview and
// consumes a bucket token only on failure. Idle peer buckets are swept every
// minute of authentication traffic after 10 minutes without an attempt.
type authFailureLimiter struct {
	rate  rate.Limit
	burst int
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*authFailureBucket
	lastSweep time.Time
	swept     bool
}

func newAuthFailureLimiter(perSecond float64, burst int) *authFailureLimiter {
	return &authFailureLimiter{
		rate:    rate.Limit(perSecond),
		burst:   burst,
		now:     time.Now,
		buckets: make(map[string]*authFailureBucket),
	}
}

// authFailurePeerAddress groups reconnecting TCP connections by remote IP, so
// changing the ephemeral source port cannot reset the bucket.
func authFailurePeerAddress(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil || p.Addr == nil {
		return unknownAuthPeer
	}
	return sourceHost(p.Addr)
}

// sourceHost keys per-source limits by host, so a new source port is the
// same source.
func sourceHost(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok && tcp.IP != nil {
		return tcp.IP.String()
	}
	// Other addresses, such as the string-backed "host:port" one
	// grpc.Server.ServeHTTP supplies, are grouped by host too, so they do not
	// all share one key.
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
		return host
	}
	return unknownAuthPeer
}

// allows reports whether the source of ctx has a failure permit left, taking
// none.
func (l *authFailureLimiter) allows(ctx context.Context) bool {
	now := l.now()
	key := authFailurePeerAddress(ctx)

	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bucketLocked(key, now).free(now)
}

func (l *authFailureLimiter) reserve(ctx context.Context) (*authFailureReservation, bool) {
	now := l.now()
	key := authFailurePeerAddress(ctx)

	l.mu.Lock()
	defer l.mu.Unlock()
	bucket := l.bucketLocked(key, now)
	if !bucket.free(now) {
		return nil, false
	}
	bucket.pending++
	return &authFailureReservation{limiter: l, bucket: bucket}, true
}

// bucketLocked returns the bucket for key, creating it. l.mu must be held.
func (l *authFailureLimiter) bucketLocked(key string, now time.Time) *authFailureBucket {
	l.sweepIdleLocked(now)

	bucket := l.buckets[key]
	if bucket == nil && len(l.buckets) >= maxAuthFailureBuckets {
		// Cardinality bound: a new peer beyond the cap shares the overflow
		// bucket instead of allocating another one.
		key = unknownAuthPeer
		bucket = l.buckets[key]
	}
	if bucket == nil {
		bucket = &authFailureBucket{limiter: rate.NewLimiter(l.rate, l.burst)}
	}
	bucket.lastSeen = now
	l.buckets[key] = bucket
	return bucket
}

// free reports whether a permit is left once the pending reviews are counted.
func (b *authFailureBucket) free(now time.Time) bool {
	return b.limiter.TokensAt(now)-float64(b.pending) >= 1
}

func (l *authFailureLimiter) sweepIdle() {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepIdleLocked(now)
}

func (l *authFailureLimiter) sweepIdleLocked(now time.Time) {
	if l.swept && now.Sub(l.lastSweep) < authFailureBucketSweepPeriod {
		return
	}
	for key, bucket := range l.buckets {
		if bucket.pending == 0 && now.Sub(bucket.lastSeen) >= authFailureBucketIdleTTL {
			delete(l.buckets, key)
		}
	}
	l.lastSweep = now
	l.swept = true
}
