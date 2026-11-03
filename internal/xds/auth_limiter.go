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
	if addr, ok := p.Addr.(*net.TCPAddr); ok && addr.IP != nil {
		return addr.IP.String()
	}
	return unknownAuthPeer
}

func (l *authFailureLimiter) reserve(ctx context.Context) (*authFailureReservation, bool) {
	now := l.now()
	key := authFailurePeerAddress(ctx)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepIdleLocked(now)

	bucket := l.buckets[key]
	if bucket == nil {
		bucket = &authFailureBucket{limiter: rate.NewLimiter(l.rate, l.burst)}
	}
	bucket.lastSeen = now
	l.buckets[key] = bucket
	if bucket.limiter.TokensAt(now)-float64(bucket.pending) < 1 {
		return nil, false
	}
	bucket.pending++
	return &authFailureReservation{limiter: l, bucket: bucket}, true
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
