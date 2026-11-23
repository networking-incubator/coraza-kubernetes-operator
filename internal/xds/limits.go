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
	"math"
	"time"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

// -----------------------------------------------------------------------------
// Constants
// -----------------------------------------------------------------------------

const (
	// DefaultMaxMessageBytes bounds a single gRPC message (16 MiB).
	DefaultMaxMessageBytes = 16 << 20
	// DefaultMaxRequestBytes bounds one ECDS request message (256 KiB). Envoy's
	// node metadata dominates the first request on a stream (about 47 KB for an
	// Istio Gateway) and grows with the Gateway pod's labels and annotations.
	DefaultMaxRequestBytes = 256 << 10
	// DefaultWriteTimeout bounds one write to a peer, so a peer that stops
	// reading is reset while an idle stream is kept.
	DefaultWriteTimeout = 30 * time.Second
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
	// DefaultAuthFailureRate bounds failed authentication attempts per second
	// from one peer IP after the burst is consumed.
	DefaultAuthFailureRate float64 = 10
	// DefaultAuthFailureBurst allows a fleet up to twice the default concurrent
	// stream cap to reconnect through one peer IP at once.
	DefaultAuthFailureBurst = 2 * DefaultMaxStreams
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
	// resourceFramingBytes bounds the ECDS resource around a configuration:
	// its name, the module URL and digest, and the VM settings.
	resourceFramingBytes = 4 << 10
	// minMaxMessageBytes is the smallest message that carries the largest
	// configuration the plugin accepts, compressed envelope included.
	minMaxMessageBytes = render.MaxConfigurationBytes + resourceFramingBytes + MessageEnvelopeBytes
)

// Limits is the serving-plane resource envelope. Flag defaults live in
// DefaultLimits; the constant-only fields are not flags.
type Limits struct {
	MaxMessageBytes           int
	MaxRequestBytes           int
	WriteTimeout              time.Duration
	MaxDecodedDataBytes       int
	MaxDataFiles              int
	MaxStreams                int
	MaxStreamsPerIdentity     int
	MaxConnectionsPerSource   int
	MaxPendingWatches         int
	MaxQueuedUpdates          int
	StreamMaxLifetime         time.Duration
	TokenReviewRevalidate     time.Duration
	AuthorizationMaxStaleness time.Duration
	TokenReviewTimeout        time.Duration
	TokenReviewRetries        int
	AuthFailureRate           float64
	AuthFailureBurst          int
}

// DefaultLimits returns the plan defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxMessageBytes:           DefaultMaxMessageBytes,
		MaxRequestBytes:           DefaultMaxRequestBytes,
		WriteTimeout:              DefaultWriteTimeout,
		MaxDecodedDataBytes:       DefaultMaxDecodedDataBytes,
		MaxDataFiles:              DefaultMaxDataFiles,
		MaxStreams:                DefaultMaxStreams,
		MaxPendingWatches:         DefaultMaxPendingWatches,
		MaxQueuedUpdates:          DefaultMaxQueuedUpdates,
		StreamMaxLifetime:         DefaultStreamMaxLifetime,
		TokenReviewRevalidate:     DefaultTokenReviewRevalidate,
		AuthorizationMaxStaleness: DefaultAuthorizationMaxStaleness,
		TokenReviewTimeout:        DefaultTokenReviewTimeout,
		TokenReviewRetries:        DefaultTokenReviewRetries,
		AuthFailureRate:           DefaultAuthFailureRate,
		AuthFailureBurst:          DefaultAuthFailureBurst,
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
	l.MaxRequestBytes = orDefault(l.MaxRequestBytes, defaults.MaxRequestBytes)
	l.WriteTimeout = orDefault(l.WriteTimeout, defaults.WriteTimeout)
	l.MaxDecodedDataBytes = orDefault(l.MaxDecodedDataBytes, defaults.MaxDecodedDataBytes)
	l.MaxDataFiles = orDefault(l.MaxDataFiles, defaults.MaxDataFiles)
	l.MaxStreams = orDefault(l.MaxStreams, defaults.MaxStreams)
	// The per-identity bound is an optional ceiling over the cap derived from
	// a ServiceAccount's authorized pods.
	l.MaxStreamsPerIdentity = orDefault(l.MaxStreamsPerIdentity, l.MaxStreams)
	// One source address may hold a 16th of the connections without an
	// admitted ECDS stream unless set; host-network Gateway pods on one node
	// share the node's address.
	l.MaxConnectionsPerSource = orDefault(l.MaxConnectionsPerSource, max(1, l.MaxStreams/perSourceShare))
	l.MaxPendingWatches = orDefault(l.MaxPendingWatches, defaults.MaxPendingWatches)
	l.MaxQueuedUpdates = orDefault(l.MaxQueuedUpdates, defaults.MaxQueuedUpdates)
	l.StreamMaxLifetime = orDefault(l.StreamMaxLifetime, defaults.StreamMaxLifetime)
	l.TokenReviewRevalidate = orDefault(l.TokenReviewRevalidate, defaults.TokenReviewRevalidate)
	l.AuthorizationMaxStaleness = orDefault(l.AuthorizationMaxStaleness, defaults.AuthorizationMaxStaleness)
	l.TokenReviewTimeout = orDefault(l.TokenReviewTimeout, defaults.TokenReviewTimeout)
	l.AuthFailureRate = orDefault(l.AuthFailureRate, defaults.AuthFailureRate)
	l.AuthFailureBurst = orDefault(l.AuthFailureBurst, defaults.AuthFailureBurst)
	// Zero retries is meaningful, so only negatives clamp; no default applies.
	l.TokenReviewRetries = max(l.TokenReviewRetries, 0)
	return l
}

// Validate rejects limits the plugin cannot honour. Zero values pass and are
// filled by WithDefaults.
func (l Limits) Validate() error {
	if math.IsNaN(l.AuthFailureRate) || math.IsInf(l.AuthFailureRate, 0) {
		return fmt.Errorf("xds-auth-failure-rate %v must be finite", l.AuthFailureRate)
	}
	l = l.WithDefaults()
	if l.AuthFailureRate <= 0 {
		return fmt.Errorf("xds-auth-failure-rate %v must be positive", l.AuthFailureRate)
	}
	if l.AuthFailureBurst <= 0 {
		return fmt.Errorf("xds-auth-failure-burst %d must be positive", l.AuthFailureBurst)
	}
	if l.TokenReviewRevalidate >= l.AuthorizationMaxStaleness {
		return fmt.Errorf("xds-token-review-revalidate %s must be less than xds-authorization-max-staleness %s", l.TokenReviewRevalidate, l.AuthorizationMaxStaleness)
	}
	if l.MaxMessageBytes > pluginMaxMessageBytes {
		return fmt.Errorf("xds-max-message-bytes %d exceeds the ECDS message cap of %d bytes", l.MaxMessageBytes, pluginMaxMessageBytes)
	}
	if l.MaxMessageBytes < minMaxMessageBytes {
		return fmt.Errorf("xds-max-message-bytes %d cannot carry the largest plugin configuration; it must be at least %d bytes", l.MaxMessageBytes, minMaxMessageBytes)
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
		fmt.Sprintf("Maximum gRPC message size in bytes for the ECDS server; at least %d, room for the plugin's 8 MiB configuration cap", minMaxMessageBytes))
	fs.IntVar(&l.MaxRequestBytes, "xds-max-request-bytes", DefaultMaxRequestBytes,
		"Maximum size in bytes of one ECDS request from a Gateway; Envoy node metadata dominates the first request on a stream and grows with Gateway pod labels and annotations")
	fs.DurationVar(&l.WriteTimeout, "xds-write-timeout", DefaultWriteTimeout,
		"Maximum time each 256 KiB of a response to an ECDS or WASM asset client may take to write, and the base of each ECDS response's total write time (plus its size at 64 KiB/s), before the stream is reset; a connection whose writes make no progress for this long is closed with all its streams")
	fs.IntVar(&l.MaxDecodedDataBytes, "xds-max-decoded-data-bytes", DefaultMaxDecodedDataBytes,
		"Maximum decoded RuleData bytes carried by one Engine (envoy-coraza accepts at most 8 MiB in the whole plugin configuration)")
	fs.IntVar(&l.MaxDataFiles, "xds-max-data-files", DefaultMaxDataFiles,
		"Maximum number of RuleData files carried by one Engine (envoy-coraza accepts at most 1024)")
	fs.IntVar(&l.MaxStreams, "xds-max-streams", DefaultMaxStreams,
		"Maximum concurrent ECDS streams per replica. Authentication slots and review permits, a quarter of this each, are shared by all sources, so one source sending many invalid tokens can delay other Gateways' token reviews until its failure budget is spent")
	fs.IntVar(&l.MaxStreamsPerIdentity, "xds-max-streams-per-identity", 0,
		"Upper bound on concurrent ECDS streams per authenticated ServiceAccount (0 means --xds-max-streams); within it a ServiceAccount may hold four streams per authorized Gateway pod running as it, plus 16. Separately, and not bounded by this flag, all ServiceAccounts of a namespace together may hold four streams per Gateway pod counted there, plus 16. Pods are counted when they carry the Gateway's label, are Running and not being deleted, are controlled by a ReplicaSet or Deployment, and name an existing ServiceAccount, so many namespaces with Gateways and Engines, or many running pods, can still use most of the streams; bound that with ResourceQuota and by who may create Engines. A scale-up to more than about three times the counted pods before they are recounted can see brief refusals, which Envoy retries")
	fs.IntVar(&l.MaxConnectionsPerSource, "xds-max-connections-per-source", 0,
		"Maximum connections from one source address that carry no admitted ECDS stream, such as module downloads; over it one of them is closed for the newest, one without a request first (0 means a 16th of --xds-max-streams). Over --xds-max-streams such connections in all, one of the source holding the most is closed; connections with an admitted ECDS stream never are. How fair this is depends on how many source addresses hold such connections, Gateways included: when sources tie, a newcomer from another source closes the oldest one without a request, which can be a Gateway's still in its handshake or before its first stream, and one with a request in progress, under token review or downloading the module only after idle ones. Host-network Gateway pods on one node share the node's address")
	fs.DurationVar(&l.StreamMaxLifetime, "xds-stream-max-lifetime", DefaultStreamMaxLifetime,
		"Maximum ECDS stream lifetime before a forced reconnect (with jitter)")
	fs.DurationVar(&l.TokenReviewRevalidate, "xds-token-review-revalidate", DefaultTokenReviewRevalidate,
		"Interval between open-stream TokenReview and authorization revalidation")
	fs.DurationVar(&l.AuthorizationMaxStaleness, "xds-authorization-max-staleness", DefaultAuthorizationMaxStaleness,
		"Maximum age of an authorization mapping before affected streams are closed")
	fs.Float64Var(&l.AuthFailureRate, "xds-auth-failure-rate", DefaultAuthFailureRate,
		"Maximum failed ECDS authentication attempts per second per peer IP")
	fs.IntVar(&l.AuthFailureBurst, "xds-auth-failure-burst", DefaultAuthFailureBurst,
		"Maximum burst of failed ECDS authentication attempts per peer IP. It also bounds how many new streams from one peer IP "+
			"can be in token review at once. The budget is per source address, so Gateways that share an address (host-network "+
			"pods on one node, or pods behind NAT) share it, and a client on that address that sends many invalid tokens can delay "+
			"those Gateways' reconnects until the budget refills; Gateways whose tokens are already cached are not affected")
}
