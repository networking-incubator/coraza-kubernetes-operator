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
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testServingCertOptions() ServingCertOptions {
	return ServingCertOptions{
		Namespace:   "coraza-system",
		Name:        "coraza-kubernetes-operator-serving-cert",
		ServiceName: "coraza-kubernetes-operator",
	}
}

func newServingCertClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the client-go scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func getServingCertSecret(t *testing.T, c client.Client, opts ServingCertOptions) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}
	if err := c.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("reading the Secret %s: %v", key, err)
	}
	return secret
}

func TestEnsureServingCertCreatesUsableMaterial(t *testing.T) {
	opts := testServingCertOptions()
	c := newServingCertClient(t)

	if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
		t.Fatalf("EnsureServingCert: %v", err)
	}

	secret := getServingCertSecret(t, c, opts)
	if secret.Type != corev1.SecretTypeTLS {
		t.Errorf("Secret type = %q, want %q", secret.Type, corev1.SecretTypeTLS)
	}

	// The listener loads the key pair from the mounted files, so the stored
	// PEM must satisfy the same parser.
	if _, err := tls.X509KeyPair(secret.Data[defaultCertName], secret.Data[defaultKeyName]); err != nil {
		t.Fatalf("the stored key pair does not load: %v", err)
	}

	// Envoy verifies the operator against the inlined CA, so the leaf must
	// chain to it for every in-cluster DNS name the Gateway may dial.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(secret.Data[servingCertCAKey]) {
		t.Fatal("the stored CA bundle holds no certificate")
	}
	block, _ := pem.Decode(secret.Data[defaultCertName])
	if block == nil {
		t.Fatal("the stored certificate is not valid PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the stored certificate: %v", err)
	}
	for _, name := range opts.dnsNames() {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err != nil {
			t.Errorf("the certificate does not verify for %q: %v", name, err)
		}
	}
}

func TestEnsureServingCertReusesExistingMaterial(t *testing.T) {
	opts := testServingCertOptions()
	c := newServingCertClient(t)

	if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
		t.Fatalf("first EnsureServingCert: %v", err)
	}
	first := getServingCertSecret(t, c, opts)

	// A second replica (or a restart) must not rotate the CA: the previous one
	// is already inlined into every rendered EnvoyFilter.
	if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
		t.Fatalf("second EnsureServingCert: %v", err)
	}
	second := getServingCertSecret(t, c, opts)

	if string(first.Data[servingCertCAKey]) != string(second.Data[servingCertCAKey]) {
		t.Error("the CA was rotated on a second call")
	}
	if string(first.Data[defaultCertName]) != string(second.Data[defaultCertName]) {
		t.Error("the serving certificate was rotated on a second call")
	}
}

func TestEnsureServingCertRegeneratesDefectiveMaterial(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{
			name:   "missing CA",
			mutate: func(s *corev1.Secret) { delete(s.Data, servingCertCAKey) },
		},
		{
			name:   "missing key",
			mutate: func(s *corev1.Secret) { delete(s.Data, defaultKeyName) },
		},
		{
			name:   "certificate is not PEM",
			mutate: func(s *corev1.Secret) { s.Data[defaultCertName] = []byte("not a certificate") },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := testServingCertOptions()
			seed, err := newServingCertSecret(opts)
			if err != nil {
				t.Fatalf("seeding the Secret: %v", err)
			}
			tc.mutate(seed)
			c := newServingCertClient(t, seed)

			if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
				t.Fatalf("EnsureServingCert: %v", err)
			}

			secret := getServingCertSecret(t, c, opts)
			if _, err := tls.X509KeyPair(secret.Data[defaultCertName], secret.Data[defaultKeyName]); err != nil {
				t.Fatalf("the regenerated key pair does not load: %v", err)
			}
			if len(secret.Data[servingCertCAKey]) == 0 {
				t.Error("the regenerated Secret has no CA bundle")
			}
		})
	}
}

func TestEnsureServingCertRegeneratesWhenSANsChange(t *testing.T) {
	opts := testServingCertOptions()
	seed, err := newServingCertSecret(opts)
	if err != nil {
		t.Fatalf("seeding the Secret: %v", err)
	}
	c := newServingCertClient(t, seed)

	// An operator renamed or given an extra SAN must re-mint, or Gateways
	// dialing the new name fail the hostname check.
	opts.ExtraDNSNames = []string{"coraza.example.com"}
	if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
		t.Fatalf("EnsureServingCert: %v", err)
	}

	secret := getServingCertSecret(t, c, opts)
	block, _ := pem.Decode(secret.Data[defaultCertName])
	if block == nil {
		t.Fatal("the regenerated certificate is not valid PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the regenerated certificate: %v", err)
	}
	if err := leaf.VerifyHostname("coraza.example.com"); err != nil {
		t.Errorf("the regenerated certificate is missing the added SAN: %v", err)
	}
}

func TestEnsureServingCertHonoursCustomKeyNames(t *testing.T) {
	opts := testServingCertOptions()
	opts.CertName = "server.crt"
	opts.KeyName = "server.key"
	c := newServingCertClient(t)

	if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
		t.Fatalf("EnsureServingCert: %v", err)
	}

	secret := getServingCertSecret(t, c, opts)
	if _, err := tls.X509KeyPair(secret.Data["server.crt"], secret.Data["server.key"]); err != nil {
		t.Fatalf("the stored key pair does not load: %v", err)
	}
	if _, found := secret.Data[defaultCertName]; found {
		t.Errorf("the Secret also holds %q; custom key names were ignored", defaultCertName)
	}
	// A kubernetes.io/tls Secret must carry tls.crt and tls.key, so custom key
	// names have to fall back to Opaque or the API server rejects the create.
	if secret.Type != corev1.SecretTypeOpaque {
		t.Errorf("Secret type = %q, want %q for custom key names", secret.Type, corev1.SecretTypeOpaque)
	}
}

func TestEnsureServingCertPreservesExistingSecretType(t *testing.T) {
	opts := testServingCertOptions()
	seed, err := newServingCertSecret(opts)
	if err != nil {
		t.Fatalf("seeding the Secret: %v", err)
	}
	// Somebody created the Secret by hand as Opaque and left out the CA.
	// Secret.type is immutable, so repair must not try to promote it to TLS.
	seed.Type = corev1.SecretTypeOpaque
	delete(seed.Data, servingCertCAKey)
	c := newServingCertClient(t, seed)

	regenerated, err := EnsureServingCert(context.Background(), c, opts)
	if err != nil {
		t.Fatalf("EnsureServingCert: %v", err)
	}
	if !regenerated {
		t.Error("EnsureServingCert did not report that it wrote new material")
	}

	secret := getServingCertSecret(t, c, opts)
	if secret.Type != corev1.SecretTypeOpaque {
		t.Errorf("Secret type = %q, want it left at %q", secret.Type, corev1.SecretTypeOpaque)
	}
	if len(secret.Data[servingCertCAKey]) == 0 {
		t.Error("the repaired Secret still has no CA bundle")
	}
}

func TestEnsureServingCertRejectsMismatchedKeyPair(t *testing.T) {
	opts := testServingCertOptions()
	seed, err := newServingCertSecret(opts)
	if err != nil {
		t.Fatalf("seeding the Secret: %v", err)
	}
	// A key from a different certificate is non-empty and parses, but the
	// listener could never load the pair.
	other, err := newServingCertSecret(opts)
	if err != nil {
		t.Fatalf("seeding the second Secret: %v", err)
	}
	seed.Data[defaultKeyName] = other.Data[defaultKeyName]
	c := newServingCertClient(t, seed)

	if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
		t.Fatalf("EnsureServingCert: %v", err)
	}

	secret := getServingCertSecret(t, c, opts)
	if _, err := tls.X509KeyPair(secret.Data[defaultCertName], secret.Data[defaultKeyName]); err != nil {
		t.Fatalf("the mismatched pair was left in place: %v", err)
	}
}

func TestEnsureServingCertRejectsUnrelatedCA(t *testing.T) {
	opts := testServingCertOptions()
	seed, err := newServingCertSecret(opts)
	if err != nil {
		t.Fatalf("seeding the Secret: %v", err)
	}
	// A CA that did not sign the leaf parses fine, but Gateways verify the
	// operator against it and would reject every connection.
	other, err := newServingCertSecret(opts)
	if err != nil {
		t.Fatalf("seeding the second Secret: %v", err)
	}
	seed.Data[servingCertCAKey] = other.Data[servingCertCAKey]
	c := newServingCertClient(t, seed)

	if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
		t.Fatalf("EnsureServingCert: %v", err)
	}

	secret := getServingCertSecret(t, c, opts)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data[servingCertCAKey]) {
		t.Fatal("the repaired Secret holds no CA bundle")
	}
	block, _ := pem.Decode(secret.Data[defaultCertName])
	if block == nil {
		t.Fatal("the repaired certificate is not valid PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing the repaired certificate: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName: opts.ServiceName, Roots: roots,
	}); err != nil {
		t.Errorf("the repaired certificate still does not chain to its CA: %v", err)
	}
}

func TestEnsureServingCertRereadsAfterConflict(t *testing.T) {
	opts := testServingCertOptions()
	seed, err := newServingCertSecret(opts)
	if err != nil {
		t.Fatalf("seeding the Secret: %v", err)
	}
	delete(seed.Data, servingCertCAKey)
	c := &conflictOnceClient{Client: newServingCertClient(t, seed)}

	// The first Update loses to another writer. A conflict is not evidence
	// that the other writer repaired anything, so the defect must be rechecked
	// rather than assumed fixed.
	if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
		t.Fatalf("EnsureServingCert: %v", err)
	}
	if c.updates < 2 {
		t.Errorf("Update was called %d time(s); the conflict was not retried", c.updates)
	}

	secret := getServingCertSecret(t, c, opts)
	if len(secret.Data[servingCertCAKey]) == 0 {
		t.Error("the Secret was left without a CA bundle after the conflict")
	}
}

// conflictOnceClient fails the first Update with a Conflict, as a competing
// writer would, and passes every later call through.
type conflictOnceClient struct {
	client.Client
	updates int
}

func (c *conflictOnceClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates++
	if c.updates == 1 {
		return apierrors.NewConflict(
			schema.GroupResource{Resource: "secrets"}, obj.GetName(), errors.New("modified"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestEnsureServingCertRejectsIncompleteOptions(t *testing.T) {
	tests := map[string]ServingCertOptions{
		"no namespace":    {Name: "s", ServiceName: "svc"},
		"no secret name":  {Namespace: "ns", ServiceName: "svc"},
		"no service name": {Namespace: "ns", Name: "s"},
	}

	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			c := newServingCertClient(t)
			if _, err := EnsureServingCert(context.Background(), c, opts); err == nil {
				t.Error("EnsureServingCert accepted incomplete options")
			}
			secrets := &corev1.SecretList{}
			if err := c.List(context.Background(), secrets); err != nil {
				t.Fatalf("listing Secrets: %v", err)
			}
			if len(secrets.Items) != 0 {
				t.Errorf("created %d Secret(s) despite invalid options", len(secrets.Items))
			}
		})
	}
}

func TestServingCertDNSNamesAreDeduplicated(t *testing.T) {
	opts := testServingCertOptions()
	opts.ExtraDNSNames = []string{"coraza-kubernetes-operator", "", "extra.example.com", "extra.example.com"}

	got := opts.dnsNames()
	want := []string{
		"coraza-kubernetes-operator",
		"coraza-kubernetes-operator.coraza-system",
		"coraza-kubernetes-operator.coraza-system.svc",
		"coraza-kubernetes-operator.coraza-system.svc.cluster.local",
		"extra.example.com",
	}
	if len(got) != len(want) {
		t.Fatalf("dnsNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dnsNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEnsureServingCertAdoptsConcurrentCreate(t *testing.T) {
	opts := testServingCertOptions()
	// Simulate the lost race: the Secret appears between the Get and the
	// Create. The fake client returns AlreadyExists, which must be tolerated.
	winner, err := newServingCertSecret(opts)
	if err != nil {
		t.Fatalf("seeding the Secret: %v", err)
	}
	c := &createRacingClient{Client: newServingCertClient(t), onCreate: winner}

	if _, err := EnsureServingCert(context.Background(), c, opts); err != nil {
		t.Fatalf("EnsureServingCert: %v", err)
	}
	secret := getServingCertSecret(t, c, opts)
	if string(secret.Data[servingCertCAKey]) != string(winner.Data[servingCertCAKey]) {
		t.Error("the winner's CA was overwritten instead of adopted")
	}
}

// createRacingClient inserts onCreate just before the first Create call, so
// that Create fails with AlreadyExists exactly as it would under a real race.
type createRacingClient struct {
	client.Client
	onCreate *corev1.Secret
}

func (c *createRacingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.onCreate != nil {
		racer := c.onCreate.DeepCopy()
		racer.ObjectMeta = metav1.ObjectMeta{Name: c.onCreate.Name, Namespace: c.onCreate.Namespace}
		c.onCreate = nil
		if err := c.Client.Create(ctx, racer); err != nil {
			return err
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}
