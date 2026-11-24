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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The operator serves ECDS and the WASM asset over TLS, and inlines its own CA
// into every EnvoyFilter it renders, so all replicas must present certificates
// from one CA. Helm mints that Secret at install time, but installs that cannot
// run Helm (OLM bundles, plain manifests, Argo CD, Flux) have no such step, and
// the Deployment would block on a Secret nobody creates.
//
// EnsureServingCert closes that gap: every replica races to create one shared
// Secret, and the loser adopts the winner's. The Secret is mounted optional so
// the pod starts before it exists; the kubelet syncs it into the volume within
// its sync period and the listener's certwatcher picks it up without a restart.

const (
	// DefaultServingCertValidity is how long a generated CA and leaf are valid.
	DefaultServingCertValidity = 365 * 24 * time.Hour

	// DefaultServingCertRenewBefore is how close to expiry a certificate may
	// get before a replica regenerates it in place.
	DefaultServingCertRenewBefore = 30 * 24 * time.Hour

	// servingCertCAKey is the Secret key holding the CA bundle. The listener
	// and the Engine reconciler read the CA from this file name, and the
	// generated Secret always uses it.
	servingCertCAKey = "ca.crt"

	// updateRetries bounds the reread-and-retry loop when another writer keeps
	// changing the Secret underneath us.
	updateRetries = 3
)

// ServingCertOptions configures EnsureServingCert.
type ServingCertOptions struct {
	// Namespace and Name identify the Secret holding the serving material.
	Namespace string
	Name      string

	// ServiceName is the operator Service; its in-cluster DNS names become the
	// certificate SANs.
	ServiceName string

	// ExtraDNSNames are appended to the derived SANs.
	ExtraDNSNames []string

	// CertName and KeyName are the Secret keys for the leaf certificate and
	// its private key. Empty values fall back to tls.crt and tls.key.
	CertName string
	KeyName  string

	// Validity and RenewBefore override the generated lifetime and the renewal
	// window. Zero values fall back to the Default* constants.
	Validity    time.Duration
	RenewBefore time.Duration

	Logger logr.Logger
}

func (o ServingCertOptions) certKey() string {
	if o.CertName != "" {
		return o.CertName
	}
	return defaultCertName
}

func (o ServingCertOptions) keyKey() string {
	if o.KeyName != "" {
		return o.KeyName
	}
	return defaultKeyName
}

func (o ServingCertOptions) validity() time.Duration {
	if o.Validity > 0 {
		return o.Validity
	}
	return DefaultServingCertValidity
}

func (o ServingCertOptions) renewBefore() time.Duration {
	if o.RenewBefore > 0 {
		return o.RenewBefore
	}
	return DefaultServingCertRenewBefore
}

// secretType reports the type a generated Secret must carry. kubernetes.io/tls
// is only legal when the material sits under the two keys the API server
// validates, so custom key names force an Opaque Secret; creating a TLS Secret
// without tls.crt and tls.key is rejected and the manager would never start.
func (o ServingCertOptions) secretType() corev1.SecretType {
	if o.certKey() == defaultCertName && o.keyKey() == defaultKeyName {
		return corev1.SecretTypeTLS
	}
	return corev1.SecretTypeOpaque
}

// dnsNames returns the SANs for the serving certificate: the Service short
// name and its three cluster-DNS forms, plus any extras, with blanks and
// duplicates removed.
func (o ServingCertOptions) dnsNames() []string {
	candidates := make([]string, 0, 4+len(o.ExtraDNSNames))
	candidates = append(candidates,
		o.ServiceName,
		fmt.Sprintf("%s.%s", o.ServiceName, o.Namespace),
		fmt.Sprintf("%s.%s.svc", o.ServiceName, o.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", o.ServiceName, o.Namespace),
	)
	candidates = append(candidates, o.ExtraDNSNames...)

	names := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

func (o ServingCertOptions) validate() error {
	switch {
	case o.Namespace == "":
		return errors.New("namespace is required")
	case o.Name == "":
		return errors.New("secret name is required")
	case o.ServiceName == "":
		return errors.New("service name is required")
	}
	return nil
}

// EnsureServingCert makes the xDS serving Secret exist and hold usable
// material, generating a self-signed CA and leaf when it does not. It reports
// whether it wrote new material.
//
// It is safe to run on every replica concurrently: the create is guarded by
// AlreadyExists and the in-place renewal rereads on conflict, so the losing
// replica adopts whatever the winner wrote. Callers should run it before the
// manager starts, with an uncached client and a bounded context.
//
// A true return means the mounted files are now stale. The listener reloads
// the leaf on its own, but CASource caches the first CA it reads, so a caller
// that regenerated a CA this startup must restart before the new CA reaches
// rendered EnvoyFilters.
//
// The namespaced RBAC it needs is declared in internal/controller/manager.go.
func EnsureServingCert(ctx context.Context, c client.Client, opts ServingCertOptions) (bool, error) {
	if err := opts.validate(); err != nil {
		return false, err
	}
	logger := opts.Logger
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}

	key := client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}

	for attempt := range updateRetries {
		existing := &corev1.Secret{}
		err := c.Get(ctx, key, existing)

		switch {
		case apierrors.IsNotFound(err):
			created, createErr := createServingCert(ctx, c, key, opts, logger)
			if apierrors.IsAlreadyExists(createErr) {
				// Another replica won the race. Loop to adopt and validate it.
				logger.Info("Adopted the xDS serving certificate created by another replica",
					"secret", key.String())
				continue
			}
			return created, createErr

		case err != nil:
			return false, fmt.Errorf("reading the xDS serving certificate Secret %s: %w", key, err)
		}

		reason := servingCertDefect(existing, opts)
		if reason == "" {
			logger.Info("Reusing the existing xDS serving certificate", "secret", key.String())
			return false, nil
		}

		logger.Info("Regenerating the xDS serving certificate",
			"secret", key.String(), "reason", reason, "attempt", attempt+1)
		updateErr := updateServingCert(ctx, c, existing, opts)
		if apierrors.IsConflict(updateErr) {
			// Someone else wrote first. Reread rather than assume they fixed
			// it: the conflicting write may have touched only an annotation
			// and left the defective material in place.
			continue
		}
		if updateErr != nil {
			return false, fmt.Errorf("updating the xDS serving certificate Secret %s: %w", key, updateErr)
		}
		return true, nil
	}

	return false, fmt.Errorf("the xDS serving certificate Secret %s kept changing under us after %d attempts",
		key, updateRetries)
}

func createServingCert(ctx context.Context, c client.Client, key client.ObjectKey,
	opts ServingCertOptions, logger logr.Logger) (bool, error) {
	secret, err := newServingCertSecret(opts)
	if err != nil {
		return false, err
	}
	if err := c.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, err
		}
		return false, fmt.Errorf("creating the xDS serving certificate Secret %s: %w", key, err)
	}
	logger.Info("Generated the xDS serving certificate",
		"secret", key.String(), "dnsNames", opts.dnsNames(),
		"notAfter", time.Now().Add(opts.validity()).UTC().Format(time.RFC3339))
	return true, nil
}

// updateServingCert replaces the material of an existing Secret in place. The
// type is left alone: Kubernetes rejects a change to Secret.type, so adopting
// the regenerated type would make every repair of a pre-existing Opaque Secret
// fail permanently.
func updateServingCert(ctx context.Context, c client.Client, existing *corev1.Secret,
	opts ServingCertOptions) error {
	regenerated, err := newServingCertSecret(opts)
	if err != nil {
		return err
	}
	if existing.Data == nil {
		existing.Data = map[string][]byte{}
	}
	for name, value := range regenerated.Data {
		existing.Data[name] = value
	}
	return c.Update(ctx, existing)
}

// servingCertDefect returns a human-readable reason the stored material cannot
// be used, or the empty string when it is fine.
func servingCertDefect(secret *corev1.Secret, opts ServingCertOptions) string {
	for _, name := range []string{servingCertCAKey, opts.certKey(), opts.keyKey()} {
		if len(secret.Data[name]) == 0 {
			return fmt.Sprintf("the Secret has no %s", name)
		}
	}

	// The listener loads the pair with the same parser, so a key that does not
	// match the certificate leaves it permanently unready.
	if _, err := tls.X509KeyPair(secret.Data[opts.certKey()], secret.Data[opts.keyKey()]); err != nil {
		return "the certificate and key do not form a usable pair"
	}

	block, _ := pem.Decode(secret.Data[opts.certKey()])
	if block == nil {
		return "the certificate is not valid PEM"
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "the certificate cannot be parsed"
	}
	if time.Now().Add(opts.renewBefore()).After(leaf.NotAfter) {
		return fmt.Sprintf("the certificate expires at %s", leaf.NotAfter.UTC().Format(time.RFC3339))
	}

	// Gateways verify the operator against the published ca.crt, so a bundle
	// that does not actually sign the leaf breaks every ECDS stream.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data[servingCertCAKey]) {
		return "the CA bundle holds no certificate"
	}
	for _, want := range opts.dnsNames() {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: want, Roots: roots}); err != nil {
			return fmt.Sprintf("the certificate does not verify against the CA bundle for %s", want)
		}
	}
	return ""
}

// newServingCertSecret mints a self-signed CA and a leaf signed by it.
func newServingCertSecret(opts ServingCertOptions) (*corev1.Secret, error) {
	caPEM, caCert, caKey, err := newSelfSignedCA(opts.Name, opts.validity())
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := newLeaf(opts.ServiceName, opts.dnsNames(), opts.validity(), caCert, caKey)
	if err != nil {
		return nil, err
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Type: opts.secretType(),
		Data: map[string][]byte{
			servingCertCAKey: caPEM,
			opts.certKey():   certPEM,
			opts.keyKey():    keyPEM,
		},
	}, nil
}

func newSelfSignedCA(name string, validity time.Duration) (caPEM []byte, caCert *x509.Certificate,
	caKey *ecdsa.PrivateKey, err error) {
	caKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generating the CA key: %w", err)
	}
	serial, err := newSerialNumber()
	if err != nil {
		return nil, nil, nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name + "-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("signing the CA certificate: %w", err)
	}
	caCert, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parsing the CA certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), caCert, caKey, nil
}

func newLeaf(commonName string, dnsNames []string, validity time.Duration,
	caCert *x509.Certificate, caKey *ecdsa.PrivateKey) (certPEM, keyPEM []byte, err error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating the serving key: %w", err)
	}
	serial, err := newSerialNumber()
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("signing the serving certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshalling the serving key: %w", err)
	}
	// The PEM copy below is what reaches the Secret; this intermediate holds
	// the same private key and is not needed once encoded.
	defer zeroize(keyDER)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		nil
}

// zeroize overwrites a buffer that held private key material rather than
// leaving it for the garbage collector.
func zeroize(buf []byte) {
	for i := range buf {
		buf[i] = 0
	}
}

func newSerialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generating a certificate serial number: %w", err)
	}
	return serial, nil
}
