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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/pki"
)

func newTestCAController(t *testing.T, objs ...runtime.Object) (*CAController, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objs {
		builder = builder.WithRuntimeObjects(obj)
	}
	client := builder.Build()
	return NewCAController(client, "test-namespace", pki.DefaultCAValidity, ctrl.Log.WithName("test")), scheme
}

func getCASecret(t *testing.T, c *CAController) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	require.NoError(t, c.client.Get(context.Background(), types.NamespacedName{
		Name: pki.CASecretName, Namespace: "test-namespace",
	}, secret))
	return secret
}

func TestCAController_EnsureCA_CreateNew(t *testing.T) {
	c, _ := newTestCAController(t)
	require.NoError(t, c.ensureCA(context.Background()))

	secret := getCASecret(t, c)
	assert.Equal(t, corev1.SecretTypeTLS, secret.Type)

	ca, err := pki.ParseCABundle(secret.Data[pki.CASecretCertKey], secret.Data[pki.CASecretKeyKey])
	require.NoError(t, err)
	assert.True(t, ca.Certificate.IsCA)
	assert.False(t, ca.IsExpired())
	assert.False(t, ca.NeedsRenewal())
}

func TestCAController_EnsureCA_ValidExisting(t *testing.T) {
	ca, err := pki.GenerateCA(pki.DefaultCAValidity)
	require.NoError(t, err)

	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pki.CASecretName, Namespace: "test-namespace"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			pki.CASecretCertKey: ca.CertPEM,
			pki.CASecretKeyKey:  ca.KeyPEM,
		},
	}
	c, _ := newTestCAController(t, existing)
	require.NoError(t, c.ensureCA(context.Background()))

	// Should not have regenerated — serial number should match.
	secret := getCASecret(t, c)
	parsed, err := pki.ParseCABundle(secret.Data[pki.CASecretCertKey], secret.Data[pki.CASecretKeyKey])
	require.NoError(t, err)
	assert.Equal(t, ca.Certificate.SerialNumber, parsed.Certificate.SerialNumber, "CA should not have been regenerated")
}

func TestCAController_EnsureCA_RenewExpiring(t *testing.T) {
	// Generate a CA that expires in 3 days (below the 5-day renewal threshold).
	expiringCA, err := pki.GenerateExpiringCA(3 * 24 * time.Hour)
	require.NoError(t, err)
	require.True(t, expiringCA.NeedsRenewal(), "test prerequisite: CA expiring in 3 days should need renewal (threshold is 5 days)")

	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pki.CASecretName, Namespace: "test-namespace"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			pki.CASecretCertKey: expiringCA.CertPEM,
			pki.CASecretKeyKey:  expiringCA.KeyPEM,
		},
	}
	c, _ := newTestCAController(t, existing)
	require.NoError(t, c.ensureCA(context.Background()))

	secret := getCASecret(t, c)
	newCA, err := pki.ParseCABundle(secret.Data[pki.CASecretCertKey], secret.Data[pki.CASecretKeyKey])
	require.NoError(t, err)

	assert.False(t, newCA.NeedsRenewal(), "renewed CA should not need renewal")
	assert.NotEqual(t, expiringCA.Certificate.SerialNumber, newCA.Certificate.SerialNumber, "CA should have been regenerated")
	assert.True(t, newCA.Certificate.NotAfter.After(expiringCA.Certificate.NotAfter), "renewed CA should expire later")
}

func TestCAController_EnsureCA_InvalidSecret(t *testing.T) {
	tests := []struct {
		name   string
		secret *corev1.Secret
	}{
		{
			name: "missing certificate",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: pki.CASecretName, Namespace: "test-namespace"},
				Type:       corev1.SecretTypeTLS,
				Data:       map[string][]byte{pki.CASecretKeyKey: []byte("fake-key")},
			},
		},
		{
			name: "missing private key",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: pki.CASecretName, Namespace: "test-namespace"},
				Type:       corev1.SecretTypeTLS,
				Data:       map[string][]byte{pki.CASecretCertKey: []byte("fake-cert")},
			},
		},
		{
			name: "invalid certificate data",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: pki.CASecretName, Namespace: "test-namespace"},
				Type:       corev1.SecretTypeTLS,
				Data: map[string][]byte{
					pki.CASecretCertKey: []byte("invalid"),
					pki.CASecretKeyKey:  []byte("invalid"),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestCAController(t, tt.secret)
			require.NoError(t, c.ensureCA(context.Background()), "should regenerate invalid CA, not fail")

			secret := getCASecret(t, c)
			_, err := pki.ParseCABundle(secret.Data[pki.CASecretCertKey], secret.Data[pki.CASecretKeyKey])
			require.NoError(t, err, "regenerated CA should be valid")
		})
	}
}

func TestCAController_GetCA(t *testing.T) {
	ca, err := pki.GenerateCA(pki.DefaultCAValidity)
	require.NoError(t, err)

	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pki.CASecretName, Namespace: "test-namespace"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			pki.CASecretCertKey: ca.CertPEM,
			pki.CASecretKeyKey:  ca.KeyPEM,
		},
	}
	c, _ := newTestCAController(t, existing)

	got, err := c.GetCA(context.Background())
	require.NoError(t, err)
	assert.True(t, got.Certificate.IsCA)
	assert.Equal(t, ca.Certificate.SerialNumber, got.Certificate.SerialNumber)
}

func TestCAController_GetCA_SecretNotFound(t *testing.T) {
	c, _ := newTestCAController(t)
	_, err := c.GetCA(context.Background())
	require.Error(t, err)
}

func TestCAController_NeedLeaderElection(t *testing.T) {
	c, _ := newTestCAController(t)
	assert.True(t, c.NeedLeaderElection())
}
