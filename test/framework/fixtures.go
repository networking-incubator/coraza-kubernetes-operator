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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// -----------------------------------------------------------------------------
// Operator Replicas
// -----------------------------------------------------------------------------

const (
	defaultHelmReleaseName = "coraza-kubernetes-operator"
	operatorChartDir       = "charts/coraza-kubernetes-operator"
)

// ScaleOperator sets the operator replica count through the Helm chart value
// and waits for the rollout. It requires the operator to have been installed by
// Helm (the integration cluster setup does this) and helm on PATH.
func (s *Scenario) ScaleOperator(operatorNamespace string, replicas int32) {
	s.T.Helper()
	require.NoError(s.T, s.scaleOperator(s.T.Context(), operatorNamespace, replicas),
		"scale operator to %d replicas", replicas)
	s.waitForOperatorReplicas(s.T.Context(), operatorNamespace, replicas)
}

// CreateSecondReplica scales the operator to two replicas (scenario 10/11) and
// restores the original replica count on cleanup.
func (s *Scenario) CreateSecondReplica(operatorNamespace string) {
	s.T.Helper()
	original := s.operatorReplicaCount(operatorNamespace)
	if original >= 2 {
		s.T.Logf("Operator already runs %d replicas", original)
		return
	}

	s.ScaleOperator(operatorNamespace, 2)
	s.OnCleanup(func() {
		if err := s.scaleOperator(context.Background(), operatorNamespace, original); err != nil {
			s.T.Logf("cleanup: failed to restore operator replicas: %v", err)
		}
	})
}

func (s *Scenario) scaleOperator(ctx context.Context, operatorNamespace string, replicas int32) error {
	release := os.Getenv("HELM_RELEASE_NAME")
	if release == "" {
		release = defaultHelmReleaseName
	}
	chartDir := os.Getenv("HELM_CHART_DIR")
	if chartDir == "" {
		chartDir = filepath.Join(repoRoot(s.T), operatorChartDir)
	}

	args := []string{
		"upgrade", release, chartDir,
		"--namespace", operatorNamespace,
		"--reuse-values",
		"--set", fmt.Sprintf("replicas=%d", replicas),
	}
	if kubeContext := s.F.KubeContext(); kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}

	out, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm %s: %w: %s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

func (s *Scenario) waitForOperatorReplicas(ctx context.Context, operatorNamespace string, replicas int32) {
	s.T.Helper()
	require.Eventually(s.T, func() bool {
		deployments, err := s.F.KubeClient.AppsV1().Deployments(operatorNamespace).List(
			ctx, metav1.ListOptions{LabelSelector: operatorPodSelector},
		)
		if err != nil || len(deployments.Items) != 1 {
			return false
		}
		dep := deployments.Items[0]
		return dep.Status.ObservedGeneration >= dep.Generation &&
			dep.Status.ReadyReplicas == replicas &&
			dep.Status.UpdatedReplicas == replicas
	}, GatewayReadyTimeout, DefaultInterval,
		"operator Deployment in %s did not reach %d ready replicas", operatorNamespace, replicas,
	)
}

func (s *Scenario) operatorReplicaCount(operatorNamespace string) int32 {
	s.T.Helper()
	deployments, err := s.F.KubeClient.AppsV1().Deployments(operatorNamespace).List(
		s.T.Context(), metav1.ListOptions{LabelSelector: operatorPodSelector},
	)
	require.NoError(s.T, err, "list operator Deployment in %s", operatorNamespace)
	require.Len(s.T, deployments.Items, 1, "expected one operator Deployment in %s", operatorNamespace)

	if deployments.Items[0].Spec.Replicas == nil {
		return 1
	}
	return *deployments.Items[0].Spec.Replicas
}

// repoRoot walks up from the test working directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err, "get working directory")

	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", dir)
		}
		dir = parent
	}
}

// -----------------------------------------------------------------------------
// External Artifact Host
// -----------------------------------------------------------------------------

const (
	externalArtifactPort          = int32(443)
	fallbackExternalArtifactImage = "nginx:1.27-alpine"
	externalArtifactCertValidity  = 24 * time.Hour
)

// ExternalArtifactHost is an in-cluster HTTPS host for custom-WASM tests
// (scenarios 21, 22 and 27). It serves the payload at /wasm/<sha256>.wasm and
// logs the request path and Authorization header.
type ExternalArtifactHost struct {
	s         *Scenario
	namespace string
	name      string
	path      string
	digest    string
	caPEM     []byte
}

// CreateExternalArtifactHost deploys an HTTPS server in namespace serving
// payload and returns a handle to its URL, CA and request logs. All resources
// are deleted on cleanup.
func (s *Scenario) CreateExternalArtifactHost(namespace, name string, payload []byte) *ExternalArtifactHost {
	s.T.Helper()
	ctx := s.T.Context()

	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	path := "/wasm/" + digest + ".wasm"
	fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace)

	caPEM, certPEM, keyPEM, err := selfSignedCert([]string{
		fqdn,
		fmt.Sprintf("%s.%s.svc", name, namespace),
		fmt.Sprintf("%s.%s", name, namespace),
		name,
	})
	require.NoError(s.T, err, "generate artifact host certificate")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-tls", Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
			"ca.crt":                caPEM,
		},
	}
	_, err = s.F.KubeClient.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(s.T, err, "create artifact host Secret %s/%s", namespace, secret.Name)

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-config", Namespace: namespace},
		Data:       map[string]string{"nginx.conf": artifactHostNginxConf(path)},
		BinaryData: map[string][]byte{"payload.wasm": payload},
	}
	_, err = s.F.KubeClient.CoreV1().ConfigMaps(namespace).Create(ctx, configMap, metav1.CreateOptions{})
	require.NoError(s.T, err, "create artifact host ConfigMap %s/%s", namespace, configMap.Name)

	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "artifact-host",
						Image: externalArtifactImage(),
						Ports: []corev1.ContainerPort{{
							Name:          "https",
							ContainerPort: externalArtifactPort,
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(externalArtifactPort)},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "tls", MountPath: "/etc/artifact-tls", ReadOnly: true},
							{
								Name:      "config",
								MountPath: "/etc/nginx/nginx.conf",
								SubPath:   "nginx.conf",
								ReadOnly:  true,
							},
							{Name: "payload", MountPath: "/etc/artifact-payload", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "tls",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{SecretName: secret.Name},
							},
						},
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: configMap.Name},
								},
							},
						},
						{
							Name: "payload",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: configMap.Name},
								},
							},
						},
					},
				},
			},
		},
	}
	_, err = s.F.KubeClient.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(s.T, err, "create artifact host Deployment %s/%s", namespace, name)

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{{
				Name:       "https",
				Port:       externalArtifactPort,
				TargetPort: intstr.FromInt32(externalArtifactPort),
			}},
		},
	}
	_, err = s.F.KubeClient.CoreV1().Services(namespace).Create(ctx, service, metav1.CreateOptions{})
	require.NoError(s.T, err, "create artifact host Service %s/%s", namespace, name)

	require.Eventually(s.T, func() bool {
		pods, listErr := s.F.KubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + name,
		})
		if listErr != nil {
			return false
		}
		for i := range pods.Items {
			if isPodReady(&pods.Items[i]) {
				return true
			}
		}
		return false
	}, DefaultTimeout, DefaultInterval, "artifact host %s/%s pod not ready", namespace, name)

	s.OnCleanup(func() {
		if err := s.F.KubeClient.AppsV1().Deployments(namespace).Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete artifact host Deployment %s/%s: %v", namespace, name, err)
		}
		if err := s.F.KubeClient.CoreV1().Services(namespace).Delete(
			context.Background(), name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete artifact host Service %s/%s: %v", namespace, name, err)
		}
		if err := s.F.KubeClient.CoreV1().ConfigMaps(namespace).Delete(
			context.Background(), configMap.Name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete artifact host ConfigMap %s/%s: %v", namespace, configMap.Name, err)
		}
		if err := s.F.KubeClient.CoreV1().Secrets(namespace).Delete(
			context.Background(), secret.Name, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete artifact host Secret %s/%s: %v", namespace, secret.Name, err)
		}
	})

	s.T.Logf("Created external artifact host: %s (%s)", fqdn, path)
	return &ExternalArtifactHost{
		s:         s,
		namespace: namespace,
		name:      name,
		path:      path,
		digest:    digest,
		caPEM:     caPEM,
	}
}

// URL returns the digest-addressed HTTPS URL as the Gateway sees it.
func (h *ExternalArtifactHost) URL() string {
	return fmt.Sprintf("https://%s.%s.svc.cluster.local%s", h.name, h.namespace, h.path)
}

// Digest returns the lowercase hex SHA-256 of the served payload.
func (h *ExternalArtifactHost) Digest() string {
	return h.digest
}

// CA returns the PEM-encoded CA that signs the host certificate. Put it in the
// EnvoyFilter's trusted_ca so Envoy verifies the host.
func (h *ExternalArtifactHost) CA() []byte {
	return h.caPEM
}

// RequestLogs returns the artifact host's access log. It records the request
// path and the Authorization header, so callers can assert that the binary
// fetch carried no bearer token (scenario 22).
func (h *ExternalArtifactHost) RequestLogs() string {
	h.s.T.Helper()
	out, err := h.s.F.Kubectl(h.namespace, "logs", "-l", "app="+h.name, "--tail=200").CombinedOutput()
	require.NoError(h.s.T, err, "read artifact host logs: %s", string(out))
	return string(out)
}

func externalArtifactImage() string {
	if image := os.Getenv("EXTERNAL_ARTIFACT_IMAGE"); image != "" {
		return image
	}
	return fallbackExternalArtifactImage
}

func artifactHostNginxConf(path string) string {
	return fmt.Sprintf(`worker_processes 1;
error_log /dev/stderr warn;
pid /tmp/nginx.pid;
events { worker_connections 64; }
http {
  log_format artifact '$request_method $request_uri auth="$http_authorization" status=$status';
  access_log /dev/stdout artifact;
  server {
    listen 443 ssl;
    server_name _;
    ssl_certificate /etc/artifact-tls/tls.crt;
    ssl_certificate_key /etc/artifact-tls/tls.key;
    location = %s {
      default_type application/wasm;
      alias /etc/artifact-payload/payload.wasm;
    }
    location / { return 404; }
  }
}
`, path)
}

// selfSignedCert returns a PEM CA chain, server certificate and EC private key.
func selfSignedCert(dnsNames []string) (caPEM, certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate private key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate serial number: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dnsNames[0]},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(externalArtifactCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caPEM = certPEM

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return caPEM, certPEM, keyPEM, nil
}

// -----------------------------------------------------------------------------
// External Egress Block
// -----------------------------------------------------------------------------

const externalEgressNetworkPolicyName = "block-external-egress"

// BlockExternalEgress applies an egress-only NetworkPolicy to the namespace's
// Gateway pods that allows only private and link-local IPv4 destinations. The
// CKO Service, kube-dns and the Istio control plane stay reachable while
// public artifact hosts do not (scenario 23).
//
// Requires a CNI that enforces NetworkPolicy; kind's default kindnet may not.
func (s *Scenario) BlockExternalEgress(namespace string) {
	s.T.Helper()

	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalEgressNetworkPolicyName,
			Namespace: namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      gatewayNameLabel,
					Operator: metav1.LabelSelectorOpExists,
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"}},
					{IPBlock: &networkingv1.IPBlock{CIDR: "172.16.0.0/12"}},
					{IPBlock: &networkingv1.IPBlock{CIDR: "192.168.0.0/16"}},
					{IPBlock: &networkingv1.IPBlock{CIDR: "127.0.0.0/8"}},
					{IPBlock: &networkingv1.IPBlock{CIDR: "169.254.0.0/16"}},
				},
			}},
		},
	}

	_, err := s.F.KubeClient.NetworkingV1().NetworkPolicies(namespace).Create(
		s.T.Context(), policy, metav1.CreateOptions{},
	)
	require.NoError(s.T, err, "create external egress block in %s", namespace)
	s.T.Logf("Blocked external egress for Gateway pods in %s", namespace)

	s.OnCleanup(func() {
		if err := s.F.KubeClient.NetworkingV1().NetworkPolicies(namespace).Delete(
			context.Background(), externalEgressNetworkPolicyName, metav1.DeleteOptions{},
		); err != nil {
			s.T.Logf("cleanup: failed to delete external egress block in %s: %v", namespace, err)
		}
	})
}
