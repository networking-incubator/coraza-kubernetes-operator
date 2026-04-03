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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/pki"
)

func newTestCertManager(t *testing.T) (*CertManager, *CAController) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	// Generate a CA and store it in a secret
	ca, err := pki.GenerateCA(pki.DefaultCAValidity)
	require.NoError(t, err)

	namespace := "test-namespace"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pki.CASecretName, Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			pki.CASecretCertKey: ca.CertPEM,
			pki.CASecretKeyKey:  ca.KeyPEM,
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	logger := ctrl.Log.WithName("test")
	caCtrl := NewCAController(client, namespace, pki.DefaultCAValidity, logger)

	certMgr := NewCertManager(caCtrl, pki.DefaultServerCertValidity, []string{"test.example.com", "localhost"}, logger)
	return certMgr, caCtrl
}

func TestCertManager_IssueCertificate(t *testing.T) {
	certMgr, _ := newTestCertManager(t)

	ctx := context.Background()
	err := certMgr.issueCertificate(ctx)
	require.NoError(t, err)

	// GetCertificate should return a valid certificate
	cert, err := certMgr.GetCertificate(nil)
	require.NoError(t, err)
	require.NotNil(t, cert)
	require.NotNil(t, cert.Leaf)

	// Verify DNS names
	assert.Contains(t, cert.Leaf.DNSNames, "test.example.com")
	assert.Contains(t, cert.Leaf.DNSNames, "localhost")

	// Verify not a CA
	assert.False(t, cert.Leaf.IsCA)

	// Verify CA PEM is available
	caPEM := certMgr.GetCAPEM()
	assert.NotEmpty(t, caPEM)
}

func TestCertManager_GetCertificate_BeforeIssue(t *testing.T) {
	certMgr, _ := newTestCertManager(t)

	// Before issuing, GetCertificate should block and then time out.
	// Use a short timeout by calling with a goroutine and checking timing.
	done := make(chan struct{})
	var cert *tls.Certificate
	var err error
	go func() {
		cert, err = certMgr.GetCertificate(nil)
		close(done)
	}()

	// Should not return within 100ms (it's blocking)
	select {
	case <-done:
		t.Fatal("GetCertificate should block when no cert is available")
	case <-time.After(100 * time.Millisecond):
		// expected: still blocking
	}

	// Issue a certificate to unblock it
	require.NoError(t, certMgr.issueCertificate(context.Background()))

	select {
	case <-done:
		require.NoError(t, err)
		require.NotNil(t, cert)
	case <-time.After(2 * time.Second):
		t.Fatal("GetCertificate did not unblock after cert was issued")
	}
}

func TestCertManager_TLSConfig(t *testing.T) {
	certMgr, _ := newTestCertManager(t)

	tlsConfig := certMgr.TLSConfig()
	require.NotNil(t, tlsConfig)
	assert.NotNil(t, tlsConfig.GetCertificate, "TLSConfig should have GetCertificate set")
	assert.Equal(t, uint16(tls.VersionTLS13), tlsConfig.MinVersion, "MinVersion should be TLS 1.3")
}

func TestCertManager_NeedLeaderElection(t *testing.T) {
	certMgr, _ := newTestCertManager(t)
	assert.True(t, certMgr.NeedLeaderElection())
}

func TestCertManager_GetCAPEM_BeforeIssue(t *testing.T) {
	certMgr, _ := newTestCertManager(t)
	assert.Nil(t, certMgr.GetCAPEM())
}
