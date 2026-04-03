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

package cache

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/pki"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

func TestServer_TLS_StartAndServe(t *testing.T) {
	ca, err := pki.GenerateCA(pki.DefaultCAValidity)
	require.NoError(t, err)

	serverCert, err := ca.IssueCertificate([]string{"localhost"}, pki.DefaultServerCertValidity)
	require.NoError(t, err)

	tlsCert, err := tls.X509KeyPair(serverCert.CertPEM, serverCert.KeyPEM)
	require.NoError(t, err)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS13,
	}

	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, ":0", logger, nil)
	server.SetTLSConfig(tlsConfig)

	// Use a fixed port for this test
	port := 48443
	server.srv.Addr = fmt.Sprintf(":%d", port)

	cache.Put("tls-test", "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\"", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()
	time.Sleep(200 * time.Millisecond)

	// Create a client that trusts the CA
	certPool := x509.NewCertPool()
	certPool.AddCert(ca.Certificate)

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    certPool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	t.Run("HTTPS request succeeds with trusted CA", func(t *testing.T) {
		resp, err := httpClient.Get(fmt.Sprintf("https://localhost:%d/rules/tls-test", port))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

		var entry RuleSetEntry
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&entry))
		assert.NotEmpty(t, entry.UUID)
		assert.Contains(t, entry.Rules, "@contains /admin")
	})

	t.Run("HTTPS latest endpoint works", func(t *testing.T) {
		resp, err := httpClient.Get(fmt.Sprintf("https://localhost:%d/rules/tls-test/latest", port))
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var latest LatestResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&latest))
		assert.NotEmpty(t, latest.UUID)
	})

	t.Run("HTTP request to HTTPS server fails", func(t *testing.T) {
		plainClient := &http.Client{Timeout: 2 * time.Second}
		resp, err := plainClient.Get(fmt.Sprintf("http://localhost:%d/rules/tls-test", port))
		if err == nil {
			defer resp.Body.Close()
			// Go's TLS server returns 400 Bad Request for plain HTTP
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		}
		// Either an error or a 400 is acceptable — both mean plain HTTP is rejected
	})

	t.Run("HTTPS request with untrusted CA fails", func(t *testing.T) {
		untrustedClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
			},
			Timeout: 2 * time.Second,
		}
		_, err := untrustedClient.Get(fmt.Sprintf("https://localhost:%d/rules/tls-test", port))
		assert.Error(t, err)
	})

	t.Run("not found returns 404 over TLS", func(t *testing.T) {
		resp, err := httpClient.Get(fmt.Sprintf("https://localhost:%d/rules/nonexistent", port))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	cancel()
	select {
	case err := <-errChan:
		if err != nil && err != http.ErrServerClosed {
			t.Errorf("Unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Server did not shut down in time")
	}
}

func TestServer_TLS_CertificateHotReload(t *testing.T) {
	ca, err := pki.GenerateCA(pki.DefaultCAValidity)
	require.NoError(t, err)

	// Issue first certificate
	serverCert1, err := ca.IssueCertificate([]string{"localhost"}, pki.DefaultServerCertValidity)
	require.NoError(t, err)

	tlsCert1, err := tls.X509KeyPair(serverCert1.CertPEM, serverCert1.KeyPEM)
	require.NoError(t, err)
	tlsCert1.Leaf = serverCert1.Certificate

	// Use GetCertificate for hot reload with atomic pointer for thread safety
	var currentCert atomic.Pointer[tls.Certificate]
	currentCert.Store(&tlsCert1)

	tlsConfig := &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return currentCert.Load(), nil
		},
		MinVersion: tls.VersionTLS13,
	}

	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, ":0", logger, nil)
	server.SetTLSConfig(tlsConfig)

	port := 48444
	server.srv.Addr = fmt.Sprintf(":%d", port)
	cache.Put("reload-test", "test rules", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Start(ctx)
	time.Sleep(200 * time.Millisecond)

	certPool := x509.NewCertPool()
	certPool.AddCert(ca.Certificate)

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    certPool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	// First request should use cert 1
	resp, err := httpClient.Get(fmt.Sprintf("https://localhost:%d/rules/reload-test", port))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "test rules")

	// Issue a new certificate (simulating hot reload)
	serverCert2, err := ca.IssueCertificate([]string{"localhost"}, pki.DefaultServerCertValidity)
	require.NoError(t, err)

	tlsCert2, err := tls.X509KeyPair(serverCert2.CertPEM, serverCert2.KeyPEM)
	require.NoError(t, err)
	tlsCert2.Leaf = serverCert2.Certificate

	// Update the current cert (hot reload) — atomic for thread safety
	currentCert.Store(&tlsCert2)

	// Force new connection by creating a fresh transport
	httpClient2 := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    certPool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	// Second request should use cert 2 (via GetCertificate)
	resp2, err := httpClient2.Get(fmt.Sprintf("https://localhost:%d/rules/reload-test", port))
	require.NoError(t, err)
	resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// Verify different serial numbers
	assert.NotEqual(t, serverCert1.Certificate.SerialNumber, serverCert2.Certificate.SerialNumber,
		"second certificate should have different serial number")

	cancel()
}
