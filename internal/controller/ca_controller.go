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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/pki"
)

// CAController manages the Certificate Authority for the operator.
// It ensures the CA secret exists, is valid, and is renewed before expiration.
// It polls every 30 seconds so deleted or corrupted secrets are restored quickly.
type CAController struct {
	client     client.Client
	namespace  string
	caValidity time.Duration
	logger     logr.Logger
}

// NewCAController creates a new CA controller instance.
func NewCAController(client client.Client, namespace string, caValidity time.Duration, logger logr.Logger) *CAController {
	return &CAController{
		client:     client,
		namespace:  namespace,
		caValidity: caValidity,
		logger:     logger.WithName("ca-controller"),
	}
}

// Start initializes the CA and runs a periodic renewal check until ctx is cancelled.
func (c *CAController) Start(ctx context.Context) error {
	c.logger.Info("Starting CA controller", "namespace", c.namespace, "caValidity", c.caValidity)

	if err := c.ensureCA(ctx); err != nil {
		return fmt.Errorf("failed to ensure CA exists: %w", err)
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("Stopping CA controller")
			return nil
		case <-ticker.C:
			c.logger.V(1).Info("Checking if CA needs renewal")
			if err := c.ensureCA(ctx); err != nil {
				c.logger.Error(err, "Failed to ensure CA during renewal check")
			}
		}
	}
}

// NeedLeaderElection implements the LeaderElectionRunnable interface.
func (c *CAController) NeedLeaderElection() bool {
	return true
}

// ensureCA ensures the CA secret exists and is valid, regenerating if needed.
func (c *CAController) ensureCA(ctx context.Context) error {
	secret := &corev1.Secret{}
	err := c.client.Get(ctx, types.NamespacedName{Name: pki.CASecretName, Namespace: c.namespace}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.logger.Info("CA secret not found, generating new CA", "secret", pki.CASecretName)
			return c.generateAndStoreCA(ctx)
		}
		return fmt.Errorf("failed to get CA secret: %w", err)
	}

	certPEM, hasCert := secret.Data[pki.CASecretCertKey]
	keyPEM, hasKey := secret.Data[pki.CASecretKeyKey]
	if !hasCert || !hasKey {
		c.logger.Info("CA secret incomplete, regenerating", "secret", pki.CASecretName, "hasCert", hasCert, "hasKey", hasKey)
		return c.generateAndStoreCA(ctx)
	}

	ca, err := pki.ParseCABundle(certPEM, keyPEM)
	if err != nil {
		c.logger.Error(err, "Failed to parse existing CA, regenerating", "secret", pki.CASecretName)
		return c.generateAndStoreCA(ctx)
	}

	if ca.IsExpired() || ca.NeedsRenewal() {
		c.logger.Info("CA needs renewal or has expired", "secret", pki.CASecretName, "notAfter", ca.Certificate.NotAfter)
		return c.generateAndStoreCA(ctx)
	}

	c.logger.V(1).Info("CA is valid", "secret", pki.CASecretName, "notAfter", ca.Certificate.NotAfter)

	// Always ensure the credential secret (cert-only) exists, even when the
	// CA itself is healthy. Covers the case where someone deletes only the
	// credential secret.
	if err := c.ensureCredentialSecret(ctx, certPEM); err != nil {
		return fmt.Errorf("failed to ensure CA credential secret: %w", err)
	}

	return nil
}

// generateAndStoreCA generates a new CA and creates or updates the secret.
func (c *CAController) generateAndStoreCA(ctx context.Context) error {
	ca, err := pki.GenerateCA(c.caValidity)
	if err != nil {
		return fmt.Errorf("failed to generate CA: %w", err)
	}

	data := map[string][]byte{
		pki.CASecretCertKey: ca.CertPEM,
		pki.CASecretKeyKey:  ca.KeyPEM,
	}

	existing := &corev1.Secret{}
	err = c.client.Get(ctx, types.NamespacedName{Name: pki.CASecretName, Namespace: c.namespace}, existing)
	if apierrors.IsNotFound(err) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pki.CASecretName,
				Namespace: c.namespace,
			},
			Type: corev1.SecretTypeTLS,
			Data: data,
		}
		if err := c.client.Create(ctx, secret); err != nil {
			return fmt.Errorf("failed to create CA secret: %w", err)
		}
		c.logger.Info("CA secret created", "notAfter", ca.Certificate.NotAfter)

		if err := c.ensureCredentialSecret(ctx, ca.CertPEM); err != nil {
			return fmt.Errorf("failed to create CA credential secret: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to check for existing CA secret: %w", err)
	}

	existing.Data = data
	if err := c.client.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update CA secret: %w", err)
	}
	c.logger.Info("CA secret updated", "notAfter", ca.Certificate.NotAfter)

	if err := c.ensureCredentialSecret(ctx, ca.CertPEM); err != nil {
		return fmt.Errorf("failed to update CA credential secret: %w", err)
	}
	return nil
}

// ensureCredentialSecret creates or updates a cert-only secret (no private key)
// for Istio DestinationRule credentialName. This prevents the CA private key
// from being exposed to the Istio control plane.
func (c *CAController) ensureCredentialSecret(ctx context.Context, caCertPEM []byte) error {
	credData := map[string][]byte{
		pki.CASecretCertKey: caCertPEM,
	}

	existing := &corev1.Secret{}
	err := c.client.Get(ctx, types.NamespacedName{Name: pki.CACredentialSecretName, Namespace: c.namespace}, existing)
	if apierrors.IsNotFound(err) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pki.CACredentialSecretName,
				Namespace: c.namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: credData,
		}
		if err := c.client.Create(ctx, secret); err != nil {
			return fmt.Errorf("failed to create CA credential secret: %w", err)
		}
		c.logger.Info("CA credential secret created (cert-only)", "secret", pki.CACredentialSecretName)
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to check for existing CA credential secret: %w", err)
	}

	existing.Data = credData
	if err := c.client.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update CA credential secret: %w", err)
	}
	c.logger.Info("CA credential secret updated (cert-only)", "secret", pki.CACredentialSecretName)
	return nil
}

// GetCA retrieves the current CA bundle from the secret.
func (c *CAController) GetCA(ctx context.Context) (*pki.CABundle, error) {
	secret := &corev1.Secret{}
	if err := c.client.Get(ctx, types.NamespacedName{Name: pki.CASecretName, Namespace: c.namespace}, secret); err != nil {
		return nil, fmt.Errorf("failed to get CA secret: %w", err)
	}

	certPEM, ok := secret.Data[pki.CASecretCertKey]
	if !ok {
		return nil, fmt.Errorf("CA secret missing certificate data")
	}
	keyPEM, ok := secret.Data[pki.CASecretKeyKey]
	if !ok {
		return nil, fmt.Errorf("CA secret missing private key data")
	}

	return pki.ParseCABundle(certPEM, keyPEM)
}
