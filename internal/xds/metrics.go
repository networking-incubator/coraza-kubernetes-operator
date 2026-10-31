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
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the bounded serving-plane metric set. Labels never carry hashes,
// nonces, tokens, proxy IDs, rule bodies, or RuleData.
type Metrics struct {
	streamsActive           prometheus.Gauge
	streamReconnects        prometheus.Counter
	streamLimitRejections   prometheus.Counter
	acks                    prometheus.Counter
	nacks                   *prometheus.CounterVec
	authFailures            *prometheus.CounterVec
	authSuccesses           prometheus.Counter
	publicationLag          prometheus.Gauge
	hashMismatch            prometheus.Counter
	coldReconstruction      prometheus.Counter
	snapshotVersionInfo     *prometheus.GaugeVec
	wasmAssetRequests       *prometheus.CounterVec
	bundledManifestVerified prometheus.Gauge
}

// NewMetrics registers the serving-plane metrics with reg.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		streamsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "coraza_xds_streams_active",
			Help: "Number of active ECDS streams served by this replica.",
		}),
		streamReconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "coraza_xds_stream_reconnects_total",
			Help: "Total ECDS streams registered, including reconnects.",
		}),
		streamLimitRejections: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "coraza_xds_stream_limit_rejections_total",
			Help: "Total ECDS streams rejected because a concurrency limit was exceeded.",
		}),
		acks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "coraza_xds_acks_total",
			Help: "Total ECDS responses acknowledged by the client.",
		}),
		nacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "coraza_xds_nacks_total",
			Help: "Total ECDS responses rejected by the client, by bounded reason.",
		}, []string{"reason"}),
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "coraza_xds_auth_failures_total",
			Help: "Total ECDS authentication and authorization failures, by bounded result.",
		}, []string{"result"}),
		authSuccesses: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "coraza_xds_auth_successes_total",
			Help: "Total ECDS streams successfully authenticated by this replica.",
		}),
		publicationLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "coraza_xds_publication_lag_seconds",
			Help: "Seconds since this replica first observed an accepted configuration it could not publish.",
		}),
		hashMismatch: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "coraza_xds_hash_mismatch_total",
			Help: "Total accepted configurations whose local reconstruction did not match.",
		}),
		coldReconstruction: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "coraza_xds_cold_reconstruction_failures_total",
			Help: "Total accepted configurations a cold replica could not reconstruct at startup.",
		}),
		snapshotVersionInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "coraza_xds_snapshot_version_info",
			Help: "Information about the snapshot served for an Engine. Value is always 1.",
		}, []string{"namespace", "engine"}),
		wasmAssetRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "coraza_wasm_asset_requests_total",
			Help: "Total WASM asset HTTP requests, by response status code.",
		}, []string{"code"}),
		bundledManifestVerified: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "coraza_bundled_manifest_verified",
			Help: "Whether every bundled WASM manifest entry exists and matches its digest (1=yes, 0=no).",
		}),
	}

	for _, collector := range []prometheus.Collector{
		m.streamsActive, m.streamReconnects, m.streamLimitRejections, m.acks,
		m.nacks, m.authFailures, m.authSuccesses,
		m.publicationLag, m.hashMismatch, m.coldReconstruction,
		m.snapshotVersionInfo, m.wasmAssetRequests, m.bundledManifestVerified,
	} {
		if err := reg.Register(collector); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// StreamOpened records a registered stream.
func (m *Metrics) StreamOpened() {
	if m == nil {
		return
	}
	m.streamsActive.Inc()
	m.streamReconnects.Inc()
}

// StreamClosed records a closed stream.
func (m *Metrics) StreamClosed() {
	if m == nil {
		return
	}
	m.streamsActive.Dec()
}

// StreamLimitRejected records a stream rejected by a concurrency limit.
func (m *Metrics) StreamLimitRejected() {
	if m == nil {
		return
	}
	m.streamLimitRejections.Inc()
}

// Ack records an acknowledged response.
func (m *Metrics) Ack() {
	if m == nil {
		return
	}
	m.acks.Inc()
}

// Nack records a rejected response. reason must be a bounded value.
func (m *Metrics) Nack(reason string) {
	if m == nil {
		return
	}
	m.nacks.WithLabelValues(reason).Inc()
}

// AuthFailure records an authentication or authorization failure. result must
// be one of "unauthorized" or "error".
func (m *Metrics) AuthFailure(result string) {
	if m == nil {
		return
	}
	m.authFailures.WithLabelValues(result).Inc()
}

// AuthSuccess records a successful stream authentication. Successes are kept
// off the failures counter so its rate stays alertable.
func (m *Metrics) AuthSuccess() {
	if m == nil {
		return
	}
	m.authSuccesses.Inc()
}

// SetPublicationLag sets the current publication lag in seconds.
func (m *Metrics) SetPublicationLag(seconds float64) {
	if m == nil {
		return
	}
	m.publicationLag.Set(seconds)
}

// HashMismatch records a reconstruction mismatch.
func (m *Metrics) HashMismatch() {
	if m == nil {
		return
	}
	m.hashMismatch.Inc()
}

// ColdReconstructionFailure records a cold-start reconstruction failure.
func (m *Metrics) ColdReconstructionFailure() {
	if m == nil {
		return
	}
	m.coldReconstruction.Inc()
}

// SetSnapshotVersion records that a snapshot is served for an Engine.
func (m *Metrics) SetSnapshotVersion(namespace, engine string) {
	if m == nil {
		return
	}
	m.snapshotVersionInfo.WithLabelValues(namespace, engine).Set(1)
}

// ForgetSnapshot removes the snapshot info series for an Engine.
func (m *Metrics) ForgetSnapshot(namespace, engine string) {
	if m == nil {
		return
	}
	m.snapshotVersionInfo.DeleteLabelValues(namespace, engine)
}

// AssetRequest records one WASM asset request by response code.
func (m *Metrics) AssetRequest(code int) {
	if m == nil {
		return
	}
	m.wasmAssetRequests.WithLabelValues(strconv.Itoa(code)).Inc()
}

// SetManifestVerified records the bundled manifest verification result.
func (m *Metrics) SetManifestVerified(verified bool) {
	if m == nil {
		return
	}
	value := 0.0
	if verified {
		value = 1
	}
	m.bundledManifestVerified.Set(value)
}
