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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

const (
	// RSAKeySize is the size of RSA keys in bits
	RSAKeySize = 4096

	// DefaultCAValidity is the default validity period for CA certificates
	DefaultCAValidity = 5 * 365 * 24 * time.Hour // 5 years

	// MinCAValidity is the minimum validity period for CA certificates
	MinCAValidity = 7 * 24 * time.Hour // 1 week

	// DefaultServerCertValidity is the default validity period for server certificates
	DefaultServerCertValidity = 30 * 24 * time.Hour // 1 month

	// MinServerCertValidity is the minimum validity period for server certificates
	MinServerCertValidity = 24 * time.Hour // 1 day

	// CASecretName is the name of the secret storing the CA certificate and key
	CASecretName = "cko-ca"

	// CASecretCertKey is the key in the secret data for the CA certificate
	CASecretCertKey = "tls.crt"

	// CASecretKeyKey is the key in the secret data for the CA private key
	CASecretKeyKey = "tls.key"

	// CACredentialSecretName is the name of the secret containing only the
	// CA certificate (no private key) for Istio DestinationRule credentialName.
	CACredentialSecretName = "cko-ca-credential"

	// CARenewalThreshold is the time before expiration when CA should be renewed
	CARenewalThreshold = 5 * 24 * time.Hour // 5 days

	// ServerCertRenewalThreshold is the time before expiration when server cert should be renewed
	ServerCertRenewalThreshold = 24 * time.Hour // 1 day
)

// CABundle contains a Certificate Authority certificate and private key
type CABundle struct {
	Certificate *x509.Certificate
	PrivateKey  *rsa.PrivateKey
	CertPEM     []byte
	KeyPEM      []byte
}

// ServerCertBundle contains a server certificate and private key
type ServerCertBundle struct {
	Certificate *x509.Certificate
	PrivateKey  *rsa.PrivateKey
	CertPEM     []byte
	KeyPEM      []byte
}

// GenerateCA creates a new Certificate Authority with the given validity period
func GenerateCA(validity time.Duration) (*CABundle, error) {
	if validity < MinCAValidity {
		return nil, fmt.Errorf("CA validity must be at least %v, got %v", MinCAValidity, validity)
	}

	// Generate private key
	privateKey, err := rsa.GenerateKey(rand.Reader, RSAKeySize)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA private key: %w", err)
	}

	// Generate serial number
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA serial number: %w", err)
	}

	// Create CA certificate template
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Coraza Kubernetes Operator"},
			CommonName:   "Coraza Kubernetes Operator CA",
		},
		NotBefore:             now,
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	// Self-sign the certificate
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	// Parse the DER certificate
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	// Encode to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	return &CABundle{
		Certificate: cert,
		PrivateKey:  privateKey,
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
	}, nil
}

// ParseCABundle parses PEM-encoded CA certificate and private key
func ParseCABundle(certPEM, keyPEM []byte) (*CABundle, error) {
	// Parse certificate
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, errors.New("failed to decode CA certificate PEM")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	if !cert.IsCA {
		return nil, errors.New("certificate is not a CA certificate")
	}

	// Parse private key
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("failed to decode CA private key PEM")
	}

	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA private key: %w", err)
	}

	return &CABundle{
		Certificate: cert,
		PrivateKey:  key,
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
	}, nil
}

// NeedsRenewal checks if the CA certificate needs to be renewed
func (ca *CABundle) NeedsRenewal() bool {
	return time.Until(ca.Certificate.NotAfter) <= CARenewalThreshold
}

// IsExpired checks if the CA certificate has expired
func (ca *CABundle) IsExpired() bool {
	return time.Now().After(ca.Certificate.NotAfter)
}

// IssueCertificate issues a server certificate signed by this CA
func (ca *CABundle) IssueCertificate(dnsNames []string, validity time.Duration) (*ServerCertBundle, error) {
	if validity < MinServerCertValidity {
		return nil, fmt.Errorf("server certificate validity must be at least %v, got %v", MinServerCertValidity, validity)
	}

	if len(dnsNames) == 0 {
		return nil, errors.New("at least one DNS name must be provided")
	}

	// Generate private key
	privateKey, err := rsa.GenerateKey(rand.Reader, RSAKeySize)
	if err != nil {
		return nil, fmt.Errorf("failed to generate server private key: %w", err)
	}

	// Generate serial number
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate server certificate serial number: %w", err)
	}

	// Create server certificate template
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Coraza Kubernetes Operator"},
			CommonName:   dnsNames[0],
		},
		DNSNames:              dnsNames,
		NotBefore:             now,
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	// Sign the certificate with the CA
	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.Certificate, &privateKey.PublicKey, ca.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create server certificate: %w", err)
	}

	// Parse the DER certificate
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse server certificate: %w", err)
	}

	// Encode to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	return &ServerCertBundle{
		Certificate: cert,
		PrivateKey:  privateKey,
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
	}, nil
}

// ParseServerCertBundle parses PEM-encoded server certificate and private key
func ParseServerCertBundle(certPEM, keyPEM []byte) (*ServerCertBundle, error) {
	// Parse certificate
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, errors.New("failed to decode server certificate PEM")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse server certificate: %w", err)
	}

	// Parse private key
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("failed to decode server private key PEM")
	}

	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse server private key: %w", err)
	}

	return &ServerCertBundle{
		Certificate: cert,
		PrivateKey:  key,
		CertPEM:     certPEM,
		KeyPEM:      keyPEM,
	}, nil
}

// NeedsRenewal checks if the server certificate needs to be renewed
func (s *ServerCertBundle) NeedsRenewal() bool {
	return time.Until(s.Certificate.NotAfter) <= ServerCertRenewalThreshold
}

// IsExpired checks if the server certificate has expired
func (s *ServerCertBundle) IsExpired() bool {
	return time.Now().After(s.Certificate.NotAfter)
}
