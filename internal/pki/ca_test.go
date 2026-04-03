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

package pki

import (
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

func TestGenerateCA(t *testing.T) {
	tests := []struct {
		name        string
		validity    time.Duration
		wantErr     bool
		errContains string
	}{
		{
			name:     "valid CA with default validity",
			validity: DefaultCAValidity,
			wantErr:  false,
		},
		{
			name:     "valid CA with minimum validity",
			validity: MinCAValidity,
			wantErr:  false,
		},
		{
			name:        "invalid CA with too short validity",
			validity:    MinCAValidity - time.Hour,
			wantErr:     true,
			errContains: "CA validity must be at least",
		},
		{
			name:     "valid CA with custom validity",
			validity: 365 * 24 * time.Hour,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ca, err := GenerateCA(tt.validity)
			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateCA() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if tt.errContains != "" && err != nil {
					if !strings.Contains(err.Error(), tt.errContains) {
						t.Errorf("GenerateCA() error = %v, should contain %v", err, tt.errContains)
					}
				}
				return
			}

			// Verify CA properties
			if ca == nil {
				t.Fatal("GenerateCA() returned nil CA bundle")
			}
			if ca.Certificate == nil {
				t.Fatal("CA certificate is nil")
			}
			if ca.PrivateKey == nil {
				t.Fatal("CA private key is nil")
			}
			if len(ca.CertPEM) == 0 {
				t.Fatal("CA certificate PEM is empty")
			}
			if len(ca.KeyPEM) == 0 {
				t.Fatal("CA private key PEM is empty")
			}

			// Verify certificate is a CA
			if !ca.Certificate.IsCA {
				t.Error("Certificate IsCA should be true")
			}

			// Verify key usage
			if ca.Certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
				t.Error("Certificate should have KeyUsageCertSign")
			}
			if ca.Certificate.KeyUsage&x509.KeyUsageCRLSign == 0 {
				t.Error("Certificate should have KeyUsageCRLSign")
			}

			// Verify validity period
			actualValidity := ca.Certificate.NotAfter.Sub(ca.Certificate.NotBefore)
			// Allow for small time differences due to generation time
			if actualValidity < tt.validity-time.Minute || actualValidity > tt.validity+time.Minute {
				t.Errorf("CA validity = %v, want %v", actualValidity, tt.validity)
			}

			// Verify subject
			if ca.Certificate.Subject.CommonName != "Coraza Kubernetes Operator CA" {
				t.Errorf("CA CommonName = %v, want 'Coraza Kubernetes Operator CA'", ca.Certificate.Subject.CommonName)
			}
		})
	}
}

func TestParseCABundle(t *testing.T) {
	// Generate a valid CA first
	originalCA, err := GenerateCA(DefaultCAValidity)
	if err != nil {
		t.Fatalf("Failed to generate CA for testing: %v", err)
	}

	tests := []struct {
		name        string
		certPEM     []byte
		keyPEM      []byte
		wantErr     bool
		errContains string
	}{
		{
			name:    "valid CA bundle",
			certPEM: originalCA.CertPEM,
			keyPEM:  originalCA.KeyPEM,
			wantErr: false,
		},
		{
			name:        "invalid certificate PEM",
			certPEM:     []byte("invalid"),
			keyPEM:      originalCA.KeyPEM,
			wantErr:     true,
			errContains: "failed to decode CA certificate PEM",
		},
		{
			name:        "invalid private key PEM",
			certPEM:     originalCA.CertPEM,
			keyPEM:      []byte("invalid"),
			wantErr:     true,
			errContains: "failed to decode CA private key PEM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ca, err := ParseCABundle(tt.certPEM, tt.keyPEM)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCABundle() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if tt.errContains != "" && err != nil {
					if !strings.Contains(err.Error(), tt.errContains) {
						t.Errorf("ParseCABundle() error = %v, should contain %v", err, tt.errContains)
					}
				}
				return
			}

			if ca == nil {
				t.Fatal("ParseCABundle() returned nil")
			}
			if ca.Certificate == nil {
				t.Error("Certificate is nil")
			}
			if ca.PrivateKey == nil {
				t.Error("PrivateKey is nil")
			}
		})
	}
}

func TestCABundle_NeedsRenewal(t *testing.T) {
	tests := []struct {
		name         string
		timeToExpiry time.Duration
		wantRenewal  bool
	}{
		{
			name:         "CA expiring soon needs renewal",
			timeToExpiry: CARenewalThreshold - time.Hour,
			wantRenewal:  true,
		},
		{
			name:         "CA not expiring soon does not need renewal",
			timeToExpiry: CARenewalThreshold + 24*time.Hour,
			wantRenewal:  false,
		},
		{
			name:         "CA at exact threshold needs renewal",
			timeToExpiry: CARenewalThreshold,
			wantRenewal:  true, // At boundary, renew to be safe
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ca, err := GenerateCA(DefaultCAValidity)
			if err != nil {
				t.Fatalf("Failed to generate CA: %v", err)
			}

			// Manually adjust NotAfter to simulate expiring certificate
			ca.Certificate.NotAfter = time.Now().Add(tt.timeToExpiry)

			if got := ca.NeedsRenewal(); got != tt.wantRenewal {
				t.Errorf("CABundle.NeedsRenewal() = %v, want %v", got, tt.wantRenewal)
			}
		})
	}
}

func TestCABundle_IsExpired(t *testing.T) {
	// Create an expired CA (backdated)
	ca, err := GenerateCA(MinCAValidity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	// Manually set NotAfter to the past
	ca.Certificate.NotAfter = time.Now().Add(-time.Hour)

	if !ca.IsExpired() {
		t.Error("CABundle.IsExpired() should return true for expired certificate")
	}

	// Create a non-expired CA
	ca2, err := GenerateCA(DefaultCAValidity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	if ca2.IsExpired() {
		t.Error("CABundle.IsExpired() should return false for valid certificate")
	}
}

func TestCABundle_IssueCertificate(t *testing.T) {
	ca, err := GenerateCA(DefaultCAValidity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	tests := []struct {
		name        string
		dnsNames    []string
		validity    time.Duration
		wantErr     bool
		errContains string
	}{
		{
			name:     "valid server certificate",
			dnsNames: []string{"test.example.com"},
			validity: DefaultServerCertValidity,
			wantErr:  false,
		},
		{
			name:     "valid server certificate with multiple SANs",
			dnsNames: []string{"test.example.com", "test.internal", "localhost"},
			validity: DefaultServerCertValidity,
			wantErr:  false,
		},
		{
			name:        "invalid - no DNS names",
			dnsNames:    []string{},
			validity:    DefaultServerCertValidity,
			wantErr:     true,
			errContains: "at least one DNS name must be provided",
		},
		{
			name:        "invalid - validity too short",
			dnsNames:    []string{"test.example.com"},
			validity:    MinServerCertValidity - time.Hour,
			wantErr:     true,
			errContains: "server certificate validity must be at least",
		},
		{
			name:     "valid - minimum validity",
			dnsNames: []string{"test.example.com"},
			validity: MinServerCertValidity,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cert, err := ca.IssueCertificate(tt.dnsNames, tt.validity)
			if (err != nil) != tt.wantErr {
				t.Errorf("IssueCertificate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if tt.errContains != "" && err != nil {
					if !strings.Contains(err.Error(), tt.errContains) {
						t.Errorf("IssueCertificate() error = %v, should contain %v", err, tt.errContains)
					}
				}
				return
			}

			// Verify certificate properties
			if cert == nil {
				t.Fatal("IssueCertificate() returned nil")
			}
			if cert.Certificate == nil {
				t.Fatal("Certificate is nil")
			}
			if cert.PrivateKey == nil {
				t.Fatal("PrivateKey is nil")
			}
			if len(cert.CertPEM) == 0 {
				t.Fatal("Certificate PEM is empty")
			}
			if len(cert.KeyPEM) == 0 {
				t.Fatal("Private key PEM is empty")
			}

			// Verify certificate is NOT a CA
			if cert.Certificate.IsCA {
				t.Error("Server certificate should not be a CA")
			}

			// Verify DNS names
			if len(cert.Certificate.DNSNames) != len(tt.dnsNames) {
				t.Errorf("DNS names count = %v, want %v", len(cert.Certificate.DNSNames), len(tt.dnsNames))
			}
			for i, name := range tt.dnsNames {
				if cert.Certificate.DNSNames[i] != name {
					t.Errorf("DNS name[%d] = %v, want %v", i, cert.Certificate.DNSNames[i], name)
				}
			}

			// Verify key usage
			if cert.Certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
				t.Error("Certificate should have KeyUsageDigitalSignature")
			}
			// KeyUsageKeyEncipherment is intentionally omitted — TLS 1.3
			// uses only ECDHE key exchange, so KeyEncipherment is not needed.

			// Verify extended key usage
			hasServerAuth := false
			for _, eku := range cert.Certificate.ExtKeyUsage {
				if eku == x509.ExtKeyUsageServerAuth {
					hasServerAuth = true
					break
				}
			}
			if !hasServerAuth {
				t.Error("Certificate should have ExtKeyUsageServerAuth")
			}

			// Verify validity period
			actualValidity := cert.Certificate.NotAfter.Sub(cert.Certificate.NotBefore)
			// Allow for small time differences
			if actualValidity < tt.validity-time.Minute || actualValidity > tt.validity+time.Minute {
				t.Errorf("Certificate validity = %v, want %v", actualValidity, tt.validity)
			}

			// Verify certificate is signed by CA
			roots := x509.NewCertPool()
			roots.AddCert(ca.Certificate)
			opts := x509.VerifyOptions{
				Roots:     roots,
				DNSName:   tt.dnsNames[0],
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}
			if _, err := cert.Certificate.Verify(opts); err != nil {
				t.Errorf("Certificate verification failed: %v", err)
			}
		})
	}
}

func TestParseServerCertBundle(t *testing.T) {
	// Generate a valid CA and server cert
	ca, err := GenerateCA(DefaultCAValidity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	originalCert, err := ca.IssueCertificate([]string{"test.example.com"}, DefaultServerCertValidity)
	if err != nil {
		t.Fatalf("Failed to generate server certificate: %v", err)
	}

	tests := []struct {
		name        string
		certPEM     []byte
		keyPEM      []byte
		wantErr     bool
		errContains string
	}{
		{
			name:    "valid server certificate bundle",
			certPEM: originalCert.CertPEM,
			keyPEM:  originalCert.KeyPEM,
			wantErr: false,
		},
		{
			name:        "invalid certificate PEM",
			certPEM:     []byte("invalid"),
			keyPEM:      originalCert.KeyPEM,
			wantErr:     true,
			errContains: "failed to decode server certificate PEM",
		},
		{
			name:        "invalid private key PEM",
			certPEM:     originalCert.CertPEM,
			keyPEM:      []byte("invalid"),
			wantErr:     true,
			errContains: "failed to decode server private key PEM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cert, err := ParseServerCertBundle(tt.certPEM, tt.keyPEM)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseServerCertBundle() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if tt.errContains != "" && err != nil {
					if !strings.Contains(err.Error(), tt.errContains) {
						t.Errorf("ParseServerCertBundle() error = %v, should contain %v", err, tt.errContains)
					}
				}
				return
			}

			if cert == nil {
				t.Fatal("ParseServerCertBundle() returned nil")
			}
			if cert.Certificate == nil {
				t.Error("Certificate is nil")
			}
			if cert.PrivateKey == nil {
				t.Error("PrivateKey is nil")
			}
		})
	}
}

func TestServerCertBundle_NeedsRenewal(t *testing.T) {
	ca, err := GenerateCA(DefaultCAValidity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	tests := []struct {
		name         string
		timeToExpiry time.Duration
		wantRenewal  bool
	}{
		{
			name:         "certificate expiring soon needs renewal",
			timeToExpiry: ServerCertRenewalThreshold - time.Hour,
			wantRenewal:  true,
		},
		{
			name:         "certificate not expiring soon does not need renewal",
			timeToExpiry: ServerCertRenewalThreshold + 24*time.Hour,
			wantRenewal:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cert, err := ca.IssueCertificate([]string{"test.example.com"}, DefaultServerCertValidity)
			if err != nil {
				t.Fatalf("Failed to generate server certificate: %v", err)
			}

			// Manually adjust NotAfter to simulate expiring certificate
			cert.Certificate.NotAfter = time.Now().Add(tt.timeToExpiry)

			if got := cert.NeedsRenewal(); got != tt.wantRenewal {
				t.Errorf("ServerCertBundle.NeedsRenewal() = %v, want %v", got, tt.wantRenewal)
			}
		})
	}
}

func TestServerCertBundle_IsExpired(t *testing.T) {
	ca, err := GenerateCA(DefaultCAValidity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	// Create an expired certificate
	cert, err := ca.IssueCertificate([]string{"test.example.com"}, MinServerCertValidity)
	if err != nil {
		t.Fatalf("Failed to generate server certificate: %v", err)
	}

	// Manually set NotAfter to the past
	cert.Certificate.NotAfter = time.Now().Add(-time.Hour)

	if !cert.IsExpired() {
		t.Error("ServerCertBundle.IsExpired() should return true for expired certificate")
	}

	// Create a non-expired certificate
	cert2, err := ca.IssueCertificate([]string{"test.example.com"}, DefaultServerCertValidity)
	if err != nil {
		t.Fatalf("Failed to generate server certificate: %v", err)
	}

	if cert2.IsExpired() {
		t.Error("ServerCertBundle.IsExpired() should return false for valid certificate")
	}
}

