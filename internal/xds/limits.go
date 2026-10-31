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
	"cmp"
	"flag"
	"fmt"
	"time"
)

// -----------------------------------------------------------------------------
// Constants
// -----------------------------------------------------------------------------

const (
	// DefaultMaxMessageBytes bounds a single gRPC message (16 MiB).
	DefaultMaxMessageBytes = 16 << 20
	// DefaultMaxDecodedDataBytes bounds the decoded RuleData carried by one
	// Engine (6 MiB). envoy-coraza carries rule_data as plain JSON strings and
	// caps the whole plugin configuration at 8 MiB, so the default leaves
	// headroom for rule text, JSON keys, and escaping.
	DefaultMaxDecodedDataBytes = 6 << 20
	// DefaultMaxDataFiles bounds the number of RuleData files per Engine
	// (envoy-coraza's hard cap).
	DefaultMaxDataFiles = 1024
	// DefaultMaxStreams bounds concurrent ECDS streams per replica.
	DefaultMaxStreams = 512
	// DefaultMaxStreamsPerIdentity bounds concurrent ECDS streams per
	// authenticated ServiceAccount; operators may lower it independently.
	DefaultMaxStreamsPerIdentity = DefaultMaxStreams
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

const (
	// MessageEnvelopeBytes reserves the DiscoveryResponse wrapper the xDS server
	// adds around a rendered resource (version_info, nonce, type_url, the
	// control-plane identifier and the Any framing). grpc.MaxSendMsgSize applies
	// to the whole message, so a resource that only fits without the wrapper is
	// Ready but undeliverable.
	MessageEnvelopeBytes = 1 << 10
	// DataFileNameBytes is the CKO API cap on a RuleData file name (and so the
	// plugin's cap on a data-file key).
	DataFileNameBytes = 253
	// dataFileKeyOverheadBytes bounds the JSON punctuation around one data-file
	// key (`"name":"content",`). Names are limited to [-._a-zA-Z0-9], so they
	// never need JSON escaping.
	dataFileKeyOverheadBytes = 8
)

// envoy-coraza NACKs a configuration above its compile-time caps, so an
// operator configured past them would leave a Ready Engine without a published
// WAF. The whole configuration is capped at 8 MiB; that exact check runs on the
// rendered payload because rule text also counts.
const (
	// The gRPC message may exceed the plugin configuration cap because it
	// carries the ECDS envelope; validateRenderedPayload checks the exact
	// payload against render.MaxConfigurationBytes.
	pluginMaxMessageBytes     = 16 << 20
	pluginMaxDecodedDataBytes = 8 << 20
	pluginMaxDataFiles        = 1024
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

// orDefault returns v when it is positive, otherwise d. Unlike cmp.Or it
// also rejects negative values, which mean "use the default" for limits.
func orDefault[T cmp.Ordered](v, d T) T {
	var zero T
	if v <= zero {
		return d
	}
	return v
}

// WithDefaults fills zero fields from DefaultLimits, so a partially set Limits
// still yields the full serving-plane envelope. Zero (or negative) means "use
// the default". Callers that accept Limits from flags or tests must apply it
// before using any field.
func (l Limits) WithDefaults() Limits {
	defaults := DefaultLimits()
	l.MaxMessageBytes = orDefault(l.MaxMessageBytes, defaults.MaxMessageBytes)
	l.MaxDecodedDataBytes = orDefault(l.MaxDecodedDataBytes, defaults.MaxDecodedDataBytes)
	l.MaxDataFiles = orDefault(l.MaxDataFiles, defaults.MaxDataFiles)
	l.MaxStreams = orDefault(l.MaxStreams, defaults.MaxStreams)
	l.MaxStreamsPerIdentity = orDefault(l.MaxStreamsPerIdentity, defaults.MaxStreamsPerIdentity)
	l.MaxPendingWatches = orDefault(l.MaxPendingWatches, defaults.MaxPendingWatches)
	l.MaxQueuedUpdates = orDefault(l.MaxQueuedUpdates, defaults.MaxQueuedUpdates)
	l.StreamMaxLifetime = orDefault(l.StreamMaxLifetime, defaults.StreamMaxLifetime)
	l.TokenReviewRevalidate = orDefault(l.TokenReviewRevalidate, defaults.TokenReviewRevalidate)
	l.AuthorizationMaxStaleness = orDefault(l.AuthorizationMaxStaleness, defaults.AuthorizationMaxStaleness)
	l.TokenReviewTimeout = orDefault(l.TokenReviewTimeout, defaults.TokenReviewTimeout)
	// Zero retries is meaningful, so only negatives clamp; no default applies.
	l.TokenReviewRetries = max(l.TokenReviewRetries, 0)
	return l
}

// Validate rejects limits the plugin cannot honour. Zero values pass and are
// filled by WithDefaults.
func (l Limits) Validate() error {
	l = l.WithDefaults()
	if l.TokenReviewRevalidate >= l.AuthorizationMaxStaleness {
		return fmt.Errorf("xds-token-review-revalidate %s must be less than xds-authorization-max-staleness %s", l.TokenReviewRevalidate, l.AuthorizationMaxStaleness)
	}
	if l.MaxMessageBytes > pluginMaxMessageBytes {
		return fmt.Errorf("xds-max-message-bytes %d exceeds the ECDS message cap of %d bytes", l.MaxMessageBytes, pluginMaxMessageBytes)
	}
	if l.MaxDecodedDataBytes > pluginMaxDecodedDataBytes {
		return fmt.Errorf("xds-max-decoded-data-bytes %d exceeds the plugin decoded RuleData cap of %d bytes", l.MaxDecodedDataBytes, pluginMaxDecodedDataBytes)
	}
	if l.MaxDataFiles > pluginMaxDataFiles {
		return fmt.Errorf("xds-max-data-files %d exceeds the plugin data file cap of %d files", l.MaxDataFiles, pluginMaxDataFiles)
	}
	// rule_data values are plain JSON strings, so the decoded bytes appear
	// roughly once, plus one maximum-length key per file and the envelope. The
	// plugin's 8 MiB configuration cap is enforced on the exact rendered
	// payload; this bound only catches a combination that cannot fit the gRPC
	// message even before rule text is counted.
	keyOverhead := l.MaxDataFiles * (DataFileNameBytes + dataFileKeyOverheadBytes)
	if required := l.MaxDecodedDataBytes + keyOverhead + MessageEnvelopeBytes; required > l.MaxMessageBytes {
		return fmt.Errorf("xds-max-decoded-data-bytes %d and xds-max-data-files %d need up to %d bytes once keyed, which exceeds xds-max-message-bytes %d", l.MaxDecodedDataBytes, l.MaxDataFiles, required, l.MaxMessageBytes)
	}
	return nil
}

// AddFlags registers the serving-plane flags on fs.
func (l *Limits) AddFlags(fs *flag.FlagSet) {
	fs.IntVar(&l.MaxMessageBytes, "xds-max-message-bytes", DefaultMaxMessageBytes,
		"Maximum gRPC message size in bytes for the ECDS server (the plugin configuration is separately capped at 8 MiB)")
	fs.IntVar(&l.MaxDecodedDataBytes, "xds-max-decoded-data-bytes", DefaultMaxDecodedDataBytes,
		"Maximum decoded RuleData bytes carried by one Engine (envoy-coraza accepts at most 8 MiB in the whole plugin configuration)")
	fs.IntVar(&l.MaxDataFiles, "xds-max-data-files", DefaultMaxDataFiles,
		"Maximum number of RuleData files carried by one Engine (envoy-coraza accepts at most 1024)")
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
