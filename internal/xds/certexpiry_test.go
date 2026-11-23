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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issueCertificate returns a certificate valid until notAfter, signed by
// parent (self-signed when parent is nil), with its DER and key.
func issueCertificate(t *testing.T, notAfter time.Time, isCA bool, parent *x509.Certificate, parentKey ed25519.PrivateKey) (*x509.Certificate, []byte, ed25519.PrivateKey) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		NotBefore:             notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:              notAfter,
		IsCA:                  isCA,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), parentKey)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, der, key
}

// expiringCertificate returns a self-signed certificate valid until notAfter,
// with its certificate and key in PEM.
func expiringCertificate(t *testing.T, notAfter time.Time) (*x509.Certificate, []byte, []byte) {
	t.Helper()
	cert, der, key := issueCertificate(t, notAfter, true, nil, nil)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return cert, pemCertificate(der), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func pemCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// current returns cert the way the certificate watcher reports it.
func current(cert *tls.Certificate) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil }
}

func newTestExpiry(t *testing.T) (*certificateExpiry, *Metrics, *strings.Builder) {
	t.Helper()
	metrics, err := NewMetrics(prometheus.NewRegistry())
	require.NoError(t, err)
	var logs strings.Builder
	return newCertificateExpiry(metrics, funcr.New(func(prefix, args string) {
		logs.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{})), metrics, &logs
}

func TestCertificateExpiryReportsGaugesAndLogs(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	tests := []struct {
		name     string
		notAfter time.Time
		wantLog  string
	}{
		{name: "expired", notAfter: now.Add(-time.Hour), wantLog: "has expired"},
		{name: "expires within 30 days", notAfter: now.Add(10 * 24 * time.Hour), wantLog: "expires soon"},
		{name: "valid for longer", notAfter: now.Add(90 * 24 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expiry, metrics, logs := newTestExpiry(t)

			leaf, _, _ := expiringCertificate(t, tt.notAfter)
			expiry.observeServingCert(current(&tls.Certificate{Leaf: leaf}))
			assert.Equal(t, float64(tt.notAfter.Unix()), testutil.ToFloat64(metrics.servingCertNotAfter))

			// No chain verifies this leaf, so the CA gauge reports the
			// earliest certificate of the bundle.
			_, laterPEM, _ := expiringCertificate(t, tt.notAfter.Add(time.Hour))
			_, earliestPEM, _ := expiringCertificate(t, tt.notAfter)
			expiry.observeCA(append(laterPEM, earliestPEM...))
			assert.Equal(t, float64(tt.notAfter.Unix()), testutil.ToFloat64(metrics.caNotAfter))

			output := logs.String()
			assert.Contains(t, output, "No certificate chain in the xDS CA bundle verifies the serving certificate")
			if tt.wantLog == "" {
				assert.NotContains(t, output, "has expired")
				assert.NotContains(t, output, "expires soon")
				return
			}
			assert.Equal(t, 2, strings.Count(output, tt.wantLog), "one line each for the certificate and the CA:\n%s", output)
			assert.Contains(t, output, `"certificate"="serving certificate"`)
			assert.Contains(t, output, `"certificate"="CA bundle"`)
		})
	}
}

// TestCertificateExpiryFollowsTheChainThatVerifies keeps an expired CA in the
// bundle next to the CA that signs the serving certificate: the CA gauge
// reports the chain that verifies the certificate, intermediates included.
func TestCertificateExpiryFollowsTheChainThatVerifies(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	_, oldDER, _ := issueCertificate(t, now.Add(-time.Hour), true, nil, nil)
	newRoot, newDER, newKey := issueCertificate(t, now.Add(365*24*time.Hour), true, nil, nil)
	intermediate, intermediateDER, intermediateKey := issueCertificate(t, now.Add(200*24*time.Hour), true, newRoot, newKey)
	bundle := append(pemCertificate(oldDER), pemCertificate(newDER)...)
	rootLeaf, rootLeafDER, _ := issueCertificate(t, now.Add(90*24*time.Hour), false, newRoot, newKey)
	signedByRoot := &tls.Certificate{Certificate: [][]byte{rootLeafDER}, Leaf: rootLeaf}
	intermediateLeaf, intermediateLeafDER, _ := issueCertificate(t, now.Add(90*24*time.Hour), false, intermediate, intermediateKey)
	signedByIntermediate := &tls.Certificate{Certificate: [][]byte{intermediateLeafDER, intermediateDER}, Leaf: intermediateLeaf}
	// Valid from 35 days from now: verified at its NotBefore.
	futureLeaf, futureLeafDER, _ := issueCertificate(t, now.Add(400*24*time.Hour), false, newRoot, newKey)
	notValidYet := &tls.Certificate{Certificate: [][]byte{futureLeafDER}, Leaf: futureLeaf}
	// An intermediate that expired two hours ago, under the valid new root.
	expiredIntermediate, expiredIntermediateDER, expiredIntermediateKey := issueCertificate(t, now.Add(-2*time.Hour), true, newRoot, newKey)
	expiredChainLeaf, expiredChainLeafDER, _ := issueCertificate(t, now.Add(90*24*time.Hour), false, expiredIntermediate, expiredIntermediateKey)
	signedByExpiredIntermediate := &tls.Certificate{Certificate: [][]byte{expiredChainLeafDER, expiredIntermediateDER}, Leaf: expiredChainLeaf}

	tests := []struct {
		name   string
		leaf   *tls.Certificate
		bundle []byte
		want   time.Time
		logs   string
	}{
		{name: "the new root signs the leaf", leaf: signedByRoot, bundle: bundle, want: newRoot.NotAfter},
		{name: "an intermediate in tls.crt expires first", leaf: signedByIntermediate, bundle: bundle, want: intermediate.NotAfter},
		{name: "a leaf not valid yet", leaf: notValidYet, bundle: bundle, want: newRoot.NotAfter},
		{name: "only the expired root is distributed", leaf: signedByRoot, bundle: pemCertificate(oldDER), want: now.Add(-time.Hour), logs: "has expired"},
		{name: "an expired intermediate in tls.crt", leaf: signedByExpiredIntermediate, bundle: pemCertificate(newDER), want: expiredIntermediate.NotAfter, logs: "has expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expiry, metrics, logs := newTestExpiry(t)
			expiry.observeCA(tt.bundle)
			expiry.observeServingCert(current(tt.leaf))
			assert.Equal(t, float64(tt.want.Unix()), testutil.ToFloat64(metrics.caNotAfter))
			if tt.logs == "" {
				assert.NotContains(t, logs.String(), "has expired")
			} else {
				assert.Contains(t, logs.String(), tt.logs)
			}
		})
	}
}

// TestCertificateExpiryRefreshReportsAgain checks the daily refresh reports
// both expiries again, recomputed.
// TestCertificateExpiryRefreshKeepsAnExpiredIntermediate refreshes after the
// intermediate in tls.crt has expired: no chain verifies any more, and the CA
// gauge stays on the intermediate's expiry rather than moving to the root's.
func TestCertificateExpiryRefreshKeepsAnExpiredIntermediate(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	root, rootDER, rootKey := issueCertificate(t, now.Add(365*24*time.Hour), true, nil, nil)
	intermediate, intermediateDER, intermediateKey := issueCertificate(t, now.Add(10*24*time.Hour), true, root, rootKey)
	leaf, leafDER, _ := issueCertificate(t, now.Add(90*24*time.Hour), false, intermediate, intermediateKey)

	expiry, metrics, logs := newTestExpiry(t)
	expiry.observeCA(pemCertificate(rootDER))
	expiry.observeServingCert(current(&tls.Certificate{Certificate: [][]byte{leafDER, intermediateDER}, Leaf: leaf}))
	require.Equal(t, float64(intermediate.NotAfter.Unix()), testutil.ToFloat64(metrics.caNotAfter))

	expiry.now = func() time.Time { return now.Add(20 * 24 * time.Hour) }
	expiry.refresh()
	assert.Equal(t, float64(intermediate.NotAfter.Unix()), testutil.ToFloat64(metrics.caNotAfter), "the gauge must not move to the root's expiry")
	assert.Contains(t, logs.String(), "has expired")
	assert.Contains(t, logs.String(), "No certificate chain in the xDS CA bundle verifies the serving certificate")
}

func TestCertificateExpiryRefreshReportsAgain(t *testing.T) {
	expiry, _, logs := newTestExpiry(t)
	leaf, leafPEM, _ := expiringCertificate(t, time.Now().Add(10*24*time.Hour))
	expiry.observeCA(leafPEM)
	expiry.observeServingCert(current(&tls.Certificate{Leaf: leaf}))
	require.Equal(t, 2, strings.Count(logs.String(), "expires soon"))
	expiry.refresh()
	assert.Equal(t, 4, strings.Count(logs.String(), "expires soon"))
	assert.NotContains(t, logs.String(), "No certificate chain")
}

// TestListenerReportsCertificateExpiryOnReload checks that the listener feeds
// the gauges from the files it loads, stays ready with an expired
// certificate, and reports the last of two quick rotations.
func TestListenerReportsCertificateExpiryOnReload(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	first, certPEM, keyPEM := expiringCertificate(t, time.Now().Add(-time.Hour).Truncate(time.Second))
	write("ca.crt", certPEM)
	write("tls.crt", certPEM)
	write("tls.key", keyPEM)

	fixture := newListenerFixtureWithConfig(t, ListenerConfig{
		BindAddress: "127.0.0.1:0",
		CertDir:     dir,
		CertName:    "tls.crt",
		KeyName:     "tls.key",
		CAPath:      filepath.Join(dir, "ca.crt"),
	}, DefaultLimits())
	metrics := fixture.server.Metrics
	gaugeIs := func(gauge prometheus.Gauge, at time.Time) func() bool {
		return func() bool { return testutil.ToFloat64(gauge) == float64(at.Unix()) }
	}
	require.Eventually(t, gaugeIs(metrics.servingCertNotAfter, first.NotAfter), 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, gaugeIs(metrics.caNotAfter, first.NotAfter), 5*time.Second, 10*time.Millisecond)
	require.NoError(t, fixture.listener.Ready(), "readiness must not depend on certificate expiry")

	var last *x509.Certificate
	for _, days := range []int{365, 400} {
		cert, nextCertPEM, nextKeyPEM := expiringCertificate(t, time.Now().Add(time.Duration(days)*24*time.Hour).Truncate(time.Second))
		write("tls.crt", nextCertPEM)
		write("tls.key", nextKeyPEM)
		last = cert
	}
	require.Eventually(t, gaugeIs(metrics.servingCertNotAfter, last.NotAfter),
		15*time.Second, 100*time.Millisecond, "the last rotated certificate must update the gauge")
	require.Never(t, func() bool { return !gaugeIs(metrics.servingCertNotAfter, last.NotAfter)() },
		500*time.Millisecond, 10*time.Millisecond, "an earlier reload must not overwrite the last certificate")
}
