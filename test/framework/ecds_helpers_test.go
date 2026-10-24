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

package framework

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEnvoyStats(t *testing.T) {
	body := []byte(`{"stats":[{"name":"server.live","value":1},{"name":"http.requests","value":42}]}`)

	stats, err := parseEnvoyStats(body)
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats["server.live"])
	assert.Equal(t, int64(42), stats["http.requests"])
}

func TestParseEnvoyStatsInvalidJSON(t *testing.T) {
	_, err := parseEnvoyStats([]byte("not-json"))
	require.Error(t, err)
}

func TestSelfSignedCert(t *testing.T) {
	wantNames := []string{"artifact.example.svc", "artifact.example.svc.cluster.local"}

	caPEM, certPEM, keyPEM, err := selfSignedCert(wantNames)
	require.NoError(t, err)
	assert.NotEmpty(t, caPEM)

	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block, "server certificate must be PEM encoded")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.True(t, cert.IsCA, "self-signed certificate must be usable as a trust anchor")
	assert.ElementsMatch(t, wantNames, cert.DNSNames)

	_, err = tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err, "certificate and key must form a valid pair")
}
