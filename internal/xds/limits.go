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

// Package xds implements the operator-side ECDS serving plane: an immutable
// snapshot store, the state-of-the-world ExtensionConfigDiscoveryService,
// bearer-token authentication, per-Engine authorization, stream revalidation,
// TLS/ALPN dispatch, the digest-addressed WASM asset handler, readiness, and
// bounded metrics.
package xds

import (
	"flag"
	"time"
)

// -----------------------------------------------------------------------------
// Constants
// -----------------------------------------------------------------------------

const (
	// DefaultMaxMessageBytes bounds a single gRPC message (16 MiB).
	DefaultMaxMessageBytes = 16 << 20
	// DefaultMaxDecodedDataBytes bounds the decoded RuleData carried by one
	// Engine (12 MiB). It must stay below the base64-expanded message limit.
	DefaultMaxDecodedDataBytes = 12 << 20
	// DefaultMaxDataFiles bounds the number of RuleData files per Engine.
	DefaultMaxDataFiles = 4096
	// DefaultMaxStreams bounds concurrent ECDS streams per replica.
	DefaultMaxStreams = 512
	// DefaultMaxStreamsPerIdentity bounds concurrent ECDS streams per
	// authenticated ServiceAccount.
	DefaultMaxStreamsPerIdentity = 8
	// DefaultMaxPendingWatches is the number of pending snapshot watches per
	// stream. Updates coalesce to the latest snapshot.
	DefaultMaxPendingWatches = 1
	// DefaultMaxQueuedUpdates is the number of queued snapshot updates per
	// stream. The latest snapshot always wins.
	DefaultMaxQueuedUpdates = 1
	// DefaultStreamMaxLifetime forces a reconnect so rotated tokens are
	// re-read.
	DefaultStreamMaxLifetime = 10 * time.Minute
	// DefaultTokenReviewRevalidate is the open-stream revalidation interval.
	DefaultTokenReviewRevalidate = 2 * time.Minute
	// DefaultAuthorizationMaxStaleness bounds how old an authorization mapping
	// may be before affected streams are closed and new subscriptions are
	// rejected.
	DefaultAuthorizationMaxStaleness = 5 * time.Minute
	// DefaultTokenReviewTimeout bounds one TokenReview call.
	DefaultTokenReviewTimeout = 5 * time.Second
	// DefaultTokenReviewRetries is the number of retries after the first
	// failed TokenReview call.
	DefaultTokenReviewRetries = 2
)

// Limits is the serving-plane resource envelope. Flag defaults live in
// DefaultLimits; the constant-only fields are not flags.
type Limits struct {
	MaxMessageBytes           int
	MaxDecodedDataBytes       int
	MaxDataFiles              int
	MaxStreams                int
	MaxStreamsPerIdentity     int
	MaxPendingWatches         int
	MaxQueuedUpdates          int
	StreamMaxLifetime         time.Duration
	TokenReviewRevalidate     time.Duration
	AuthorizationMaxStaleness time.Duration
	TokenReviewTimeout        time.Duration
	TokenReviewRetries        int
}

// DefaultLimits returns the plan defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxMessageBytes:           DefaultMaxMessageBytes,
		MaxDecodedDataBytes:       DefaultMaxDecodedDataBytes,
		MaxDataFiles:              DefaultMaxDataFiles,
		MaxStreams:                DefaultMaxStreams,
		MaxStreamsPerIdentity:     DefaultMaxStreamsPerIdentity,
		MaxPendingWatches:         DefaultMaxPendingWatches,
		MaxQueuedUpdates:          DefaultMaxQueuedUpdates,
		StreamMaxLifetime:         DefaultStreamMaxLifetime,
		TokenReviewRevalidate:     DefaultTokenReviewRevalidate,
		AuthorizationMaxStaleness: DefaultAuthorizationMaxStaleness,
		TokenReviewTimeout:        DefaultTokenReviewTimeout,
		TokenReviewRetries:        DefaultTokenReviewRetries,
	}
}

// withDefaults fills zero fields from DefaultLimits. It keeps programmatic
// construction in tests honest without requiring every caller to spell out the
// full envelope.
func (l Limits) withDefaults() Limits {
	defaults := DefaultLimits()
	if l.MaxMessageBytes <= 0 {
		l.MaxMessageBytes = defaults.MaxMessageBytes
	}
	if l.MaxDecodedDataBytes <= 0 {
		l.MaxDecodedDataBytes = defaults.MaxDecodedDataBytes
	}
	if l.MaxDataFiles <= 0 {
		l.MaxDataFiles = defaults.MaxDataFiles
	}
	if l.MaxStreams <= 0 {
		l.MaxStreams = defaults.MaxStreams
	}
	if l.MaxStreamsPerIdentity <= 0 {
		l.MaxStreamsPerIdentity = defaults.MaxStreamsPerIdentity
	}
	if l.MaxPendingWatches <= 0 {
		l.MaxPendingWatches = defaults.MaxPendingWatches
	}
	if l.MaxQueuedUpdates <= 0 {
		l.MaxQueuedUpdates = defaults.MaxQueuedUpdates
	}
	if l.StreamMaxLifetime <= 0 {
		l.StreamMaxLifetime = defaults.StreamMaxLifetime
	}
	if l.TokenReviewRevalidate <= 0 {
		l.TokenReviewRevalidate = defaults.TokenReviewRevalidate
	}
	if l.AuthorizationMaxStaleness <= 0 {
		l.AuthorizationMaxStaleness = defaults.AuthorizationMaxStaleness
	}
	if l.TokenReviewTimeout <= 0 {
		l.TokenReviewTimeout = defaults.TokenReviewTimeout
	}
	if l.TokenReviewRetries < 0 {
		l.TokenReviewRetries = 0
	}
	return l
}

// AddFlags registers the serving-plane flags on fs.
func (l *Limits) AddFlags(fs *flag.FlagSet) {
	fs.IntVar(&l.MaxMessageBytes, "xds-max-message-bytes", DefaultMaxMessageBytes,
		"Maximum gRPC message size in bytes for the ECDS server")
	fs.IntVar(&l.MaxDecodedDataBytes, "xds-max-decoded-data-bytes", DefaultMaxDecodedDataBytes,
		"Maximum decoded RuleData bytes carried by one Engine")
	fs.IntVar(&l.MaxDataFiles, "xds-max-data-files", DefaultMaxDataFiles,
		"Maximum number of RuleData files carried by one Engine")
	fs.IntVar(&l.MaxStreams, "xds-max-streams", DefaultMaxStreams,
		"Maximum concurrent ECDS streams per replica")
	fs.IntVar(&l.MaxStreamsPerIdentity, "xds-max-streams-per-identity", DefaultMaxStreamsPerIdentity,
		"Maximum concurrent ECDS streams per authenticated ServiceAccount")
	fs.DurationVar(&l.StreamMaxLifetime, "xds-stream-max-lifetime", DefaultStreamMaxLifetime,
		"Maximum ECDS stream lifetime before a forced reconnect (with jitter)")
	fs.DurationVar(&l.TokenReviewRevalidate, "xds-token-review-revalidate", DefaultTokenReviewRevalidate,
		"Interval between open-stream TokenReview and authorization revalidation")
	fs.DurationVar(&l.AuthorizationMaxStaleness, "xds-authorization-max-staleness", DefaultAuthorizationMaxStaleness,
		"Maximum age of an authorization mapping before affected streams are closed")
}
