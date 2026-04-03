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

package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/pki"
)

// CertManager manages server certificates issued by the CA controller.
// It issues certificates on startup and periodically renews them before
// expiration. It provides a GetCertificate callback for hot-reload in
// the TLS server without restart.
//
// It also detects CA rotation: if the CA serial number changes between
// renewal checks, the server certificate is re-issued immediately to
// prevent Envoy from rejecting the old-CA-signed cert.
//
// GetCertificate blocks until the first certificate is available,
// preventing TLS handshake failures during startup.
type CertManager struct {
	caController       *CAController
	serverCertValidity time.Duration
	dnsNames           []string
	logger             logr.Logger

	mu           sync.RWMutex
	tlsCert      *tls.Certificate
	caPEM        []byte
	caSerial     string // serial number of the CA that signed the current server cert
	ready        chan struct{} // closed when first cert is issued
	readyOnce    sync.Once
}

// NewCertManager creates a new server certificate manager.
func NewCertManager(caController *CAController, serverCertValidity time.Duration, dnsNames []string, logger logr.Logger) *CertManager {
	return &CertManager{
		caController:       caController,
		serverCertValidity: serverCertValidity,
		dnsNames:           dnsNames,
		logger:             logger.WithName("cert-manager"),
		ready:              make(chan struct{}),
	}
}

// Start issues the initial server certificate and runs a renewal loop.
func (m *CertManager) Start(ctx context.Context) error {
	m.logger.Info("Starting certificate manager", "dnsNames", m.dnsNames, "validity", m.serverCertValidity)

	if err := m.issueCertificate(ctx); err != nil {
		return fmt.Errorf("failed to issue initial server certificate: %w", err)
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("Stopping certificate manager")
			return nil
		case <-ticker.C:
			m.mu.RLock()
			cert := m.tlsCert
			currentCASerial := m.caSerial
			m.mu.RUnlock()

			if cert == nil {
				m.logger.Info("No server certificate loaded, issuing new one")
				if err := m.issueCertificate(ctx); err != nil {
					m.logger.Error(err, "Failed to issue server certificate")
				}
				continue
			}

			leaf := cert.Leaf
			if leaf == nil {
				m.logger.Info("Certificate leaf is nil, re-issuing")
				if err := m.issueCertificate(ctx); err != nil {
					m.logger.Error(err, "Failed to re-issue server certificate")
				}
				continue
			}

			// Check if the CA has rotated — if so, re-issue immediately to
			// prevent Envoy from rejecting a cert signed by the old CA.
			ca, err := m.caController.GetCA(ctx)
			if err != nil {
				m.logger.Error(err, "Failed to get CA for rotation check")
				continue
			}
			if ca.Certificate.SerialNumber.String() != currentCASerial {
				m.logger.Info("CA has rotated, re-issuing server certificate immediately",
					"oldCASerial", currentCASerial,
					"newCASerial", ca.Certificate.SerialNumber.String(),
				)
				if err := m.issueCertificate(ctx); err != nil {
					m.logger.Error(err, "Failed to re-issue server certificate after CA rotation")
				}
				continue
			}

			if time.Until(leaf.NotAfter) <= pki.ServerCertRenewalThreshold {
				m.logger.Info("Server certificate nearing expiration, renewing",
					"notAfter", leaf.NotAfter,
					"remaining", time.Until(leaf.NotAfter),
					"threshold", pki.ServerCertRenewalThreshold,
				)
				if err := m.issueCertificate(ctx); err != nil {
					m.logger.Error(err, "Failed to renew server certificate")
				}
			}
		}
	}
}

// NeedLeaderElection implements the LeaderElectionRunnable interface.
func (m *CertManager) NeedLeaderElection() bool {
	return true
}

// issueCertificate gets the CA and issues a new server certificate.
func (m *CertManager) issueCertificate(ctx context.Context) error {
	ca, err := m.caController.GetCA(ctx)
	if err != nil {
		return fmt.Errorf("failed to get CA: %w", err)
	}

	serverCert, err := ca.IssueCertificate(m.dnsNames, m.serverCertValidity)
	if err != nil {
		return fmt.Errorf("failed to issue server certificate: %w", err)
	}

	tlsCert, err := tls.X509KeyPair(serverCert.CertPEM, serverCert.KeyPEM)
	if err != nil {
		return fmt.Errorf("failed to create TLS keypair: %w", err)
	}
	tlsCert.Leaf = serverCert.Certificate

	m.mu.Lock()
	m.tlsCert = &tlsCert
	m.caPEM = ca.CertPEM
	m.caSerial = ca.Certificate.SerialNumber.String()
	m.mu.Unlock()

	m.readyOnce.Do(func() { close(m.ready) })

	m.logger.Info("Server certificate issued",
		"dnsNames", m.dnsNames,
		"notAfter", serverCert.Certificate.NotAfter,
		"serialNumber", serverCert.Certificate.SerialNumber,
	)
	return nil
}

// GetCertificate returns the current server certificate for TLS handshakes.
// This is used as the tls.Config.GetCertificate callback.
// On the first call before a certificate is available, it blocks until
// the certificate manager has issued the initial certificate (or 30s timeout).
func (m *CertManager) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	select {
	case <-m.ready:
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("timed out waiting for server certificate to become available")
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tlsCert, nil
}

// TLSConfig returns a TLS configuration that uses GetCertificate for hot-reload.
func (m *CertManager) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: m.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	}
}

// GetCAPEM returns the current CA certificate PEM data. This is used for
// injecting the CA into engine namespaces.
func (m *CertManager) GetCAPEM() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.caPEM
}
