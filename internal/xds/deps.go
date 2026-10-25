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
	"fmt"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

// Config is the serving-plane construction input.
type Config struct {
	KubeClient    kubernetes.Interface
	Reader        client.Reader
	Cache         cache.Cache
	BindAddress   string
	TokenAudience string
	CertDir       string
	CertName      string
	KeyName       string
	CAPath        string
	Limits        Limits
	Registry      prometheus.Registerer
	Logger        logr.Logger
}

// Dependencies is the serving plane constructed once in main and shared with
// the publication controller.
type Dependencies struct {
	Metrics   *Metrics
	Store     *SnapshotStore
	Authz     *AuthzIndex
	Auth      *Authenticator
	Streams   *StreamRegistry
	Server    *Server
	Listener  *Listener
	Readiness *PublicationReadiness
	Startup   *PublicationStartup
	Limits    Limits

	assetsErr error
}

// NewDependencies builds the ECDS serving plane.
func NewDependencies(config Config) (*Dependencies, error) {
	limits := config.Limits.withDefaults()
	logger := config.Logger
	if logger.GetSink() == nil {
		logger = logr.Discard()
	}

	metrics, err := NewMetrics(config.Registry)
	if err != nil {
		return nil, fmt.Errorf("register xDS metrics: %w", err)
	}

	store := NewSnapshotStore()
	authz := NewAuthzIndex(config.Reader, limits.AuthorizationMaxStaleness)
	auth := NewAuthenticator(config.KubeClient.AuthenticationV1().TokenReviews(), config.TokenAudience,
		limits.TokenReviewRevalidate, limits.StreamMaxLifetime, limits.TokenReviewTimeout, limits.TokenReviewRetries)
	streams := NewStreamRegistry(auth, authz, limits, metrics)
	authz.SetRevoker(streams)

	server := &Server{
		Store:   store,
		Auth:    auth,
		Authz:   authz,
		Streams: streams,
		Limits:  limits,
		Metrics: metrics,
		Log:     logger.WithName("xds-server"),
	}

	manifest, data, assetErr := assets.ResolveDefault()
	metrics.SetManifestVerified(assetErr == nil)
	if assetErr != nil {
		logger.Error(assetErr, "Bundled WASM manifest could not be verified")
	}

	if err := VerifyBundledManifest(manifest, data); err != nil && assetErr == nil {
		assetErr = err
		metrics.SetManifestVerified(false)
		logger.Error(err, "Bundled WASM manifest could not be verified")
	}

	listener, err := NewListener(ListenerConfig{
		BindAddress: config.BindAddress,
		CertDir:     config.CertDir,
		CertName:    config.CertName,
		KeyName:     config.KeyName,
		CAPath:      config.CAPath,
	}, server, NewAssetHandler(manifest, data, metrics), limits, logger.WithName("xds-listener"))
	if err != nil {
		return nil, err
	}

	readiness := NewPublicationReadiness()

	return &Dependencies{
		Metrics:   metrics,
		Store:     store,
		Authz:     authz,
		Auth:      auth,
		Streams:   streams,
		Server:    server,
		Listener:  listener,
		Readiness: readiness,
		Startup: &PublicationStartup{
			Cache:     config.Cache,
			Reader:    config.Reader,
			Readiness: readiness,
		},
		Limits:    limits,
		assetsErr: assetErr,
	}, nil
}

// CheckAssets is the "xds-assets" readiness check.
func (d *Dependencies) CheckAssets() error {
	return d.assetsErr
}

// CheckServer is the "xds-server" readiness check.
func (d *Dependencies) CheckServer() error {
	return d.Listener.Ready()
}

// CheckPublication is the "xds-publication" readiness check.
func (d *Dependencies) CheckPublication() error {
	return d.Readiness.Check()
}
