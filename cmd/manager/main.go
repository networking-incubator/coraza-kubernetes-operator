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

package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/controller"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/pki"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
	// +kubebuilder:scaffold:imports
)

// -----------------------------------------------------------------------------
// Scheme Registration
// -----------------------------------------------------------------------------

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(wafv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// -----------------------------------------------------------------------------
// Vars
// -----------------------------------------------------------------------------

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func main() {
	cfg := parseFlags()
	validateFlags(cfg)

	tlsOpts := buildTLSOpts(cfg.enableHTTP2)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                buildMetricsServerOptions(cfg, tlsOpts),
		WebhookServer:          setupWebhookServer(cfg, tlsOpts),
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.enableLeaderElect,
		LeaderElectionID:       "waf.k8s.coraza.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	podNamespace := os.Getenv("POD_NAMESPACE")
	tlsConfig := setupTLSInfrastructure(mgr, cfg, podNamespace)
	rulesetCache := setupCacheServer(mgr, cfg, tlsConfig)
	setupIstioPrerequisites(mgr, cfg, podNamespace)

	if err := controller.SetupControllers(mgr, rulesetCache, cfg.envoyClusterName, cfg.istioRevision); err != nil {
		setupLog.Error(err, "unable to setup controllers")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	setupHealthChecks(mgr)

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

type config struct {
	metricsAddr       string
	probeAddr         string
	enableLeaderElect bool
	secureMetrics     bool
	enableHTTP2       bool
	metricsCertPath   string
	metricsCertName   string
	metricsCertKey    string
	webhookCertPath   string
	webhookCertName   string
	webhookCertKey    string
	cacheGCInterval   time.Duration
	cacheMaxAge       time.Duration
	cacheMaxSize      int
	cacheServerPort   int
	envoyClusterName  string
	istioRevision     string
	operatorName      string
	caValidity         time.Duration
	serverCertValidity time.Duration
	cacheCertPath      string
	cacheKeyPath       string
}

func parseFlags() config {
	var cfg config

	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&cfg.enableLeaderElect, "leader-elect", false, "Enable leader election for controller manager. "+
		"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&cfg.secureMetrics, "metrics-secure", true, "If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&cfg.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&cfg.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&cfg.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&cfg.metricsCertPath, "metrics-cert-path", "", "The directory that contains the metrics server certificate.")
	flag.StringVar(&cfg.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&cfg.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&cfg.enableHTTP2, "enable-http2", false, "If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.DurationVar(&cfg.cacheGCInterval, "cache-gc-interval", cache.CacheGCInterval, "How often to check for and remove stale cache entries in the RuleSet cache")
	flag.DurationVar(&cfg.cacheMaxAge, "cache-max-age", cache.CacheMaxAge, "Maximum age of a cache entry before it's considered stale in the RuleSet cache")
	flag.IntVar(&cfg.cacheMaxSize, "cache-max-size", cache.CacheMaxSize, fmt.Sprintf("Maximum total size of all cached rules in the RuleSet cache in bytes (default %dMB)", cache.CacheMaxSize/(1024*1024)))
	flag.IntVar(&cfg.cacheServerPort, "cache-server-port", controller.DefaultRuleSetCacheServerPort, fmt.Sprintf("Port number for the RuleSet cache server to listen on (default %d)", controller.DefaultRuleSetCacheServerPort))
	flag.StringVar(&cfg.envoyClusterName, "envoy-cluster-name", "", "The Envoy cluster name pointing to the RuleSet cache server (required)")
	flag.StringVar(&cfg.istioRevision, "istio-revision", "", "The Istio revision label value for managed Istio resources")
	flag.StringVar(&cfg.operatorName, "operator-name", "", "The operator release name used to derive managed resource names (when unset, Istio prerequisites are skipped)")
	flag.DurationVar(&cfg.caValidity, "ca-validity", pki.DefaultCAValidity, fmt.Sprintf("Validity period for the CA certificate (default %v, minimum %v)", pki.DefaultCAValidity, pki.MinCAValidity))
	flag.DurationVar(&cfg.serverCertValidity, "server-cert-validity", pki.DefaultServerCertValidity, fmt.Sprintf("Validity period for server certificates (default %v, minimum %v)", pki.DefaultServerCertValidity, pki.MinServerCertValidity))
	flag.StringVar(&cfg.cacheCertPath, "cache-cert-path", "", "Path to a user-provided TLS certificate for the cache server (disables automatic certificate management)")
	flag.StringVar(&cfg.cacheKeyPath, "cache-key-path", "", "Path to a user-provided TLS private key for the cache server (disables automatic certificate management)")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	return cfg
}

func buildTLSOpts(enableHTTP2 bool) []func(*tls.Config) {
	if enableHTTP2 {
		return nil
	}

	// Disabling http/2 prevents vulnerability to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	return []func(*tls.Config){
		func(c *tls.Config) {
			setupLog.Info("disabling http/2")
			c.NextProtos = []string{"http/1.1"}
		},
	}
}

func buildMetricsServerOptions(cfg config, tlsOpts []func(*tls.Config)) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   cfg.metricsAddr,
		SecureServing: cfg.secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if cfg.secureMetrics {
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(cfg.metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", cfg.metricsCertPath, "metrics-cert-name", cfg.metricsCertName, "metrics-cert-key", cfg.metricsCertKey)

		opts.CertDir = cfg.metricsCertPath
		opts.CertName = cfg.metricsCertName
		opts.KeyName = cfg.metricsCertKey
	}

	return opts
}

// -----------------------------------------------------------------------------
// Manager Setup
// -----------------------------------------------------------------------------

func setupWebhookServer(cfg config, tlsOpts []func(*tls.Config)) webhook.Server {
	opts := webhook.Options{TLSOpts: tlsOpts}

	if len(cfg.webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", cfg.webhookCertPath, "webhook-cert-name", cfg.webhookCertName, "webhook-cert-key", cfg.webhookCertKey)

		opts.CertDir = cfg.webhookCertPath
		opts.CertName = cfg.webhookCertName
		opts.KeyName = cfg.webhookCertKey
	}

	return webhook.NewServer(opts)
}

func setupTLSInfrastructure(mgr ctrl.Manager, cfg config, podNamespace string) *tls.Config {
	// User-provided certificate mode
	if cfg.cacheCertPath != "" && cfg.cacheKeyPath != "" {
		setupLog.Info("WARNING: Using user-provided certificate for cache server TLS. "+
			"Automatic certificate management (CA controller and cert renewal) is DISABLED. "+
			"You are responsible for certificate rotation.",
			"certPath", cfg.cacheCertPath, "keyPath", cfg.cacheKeyPath,
		)
		cert, err := tls.LoadX509KeyPair(cfg.cacheCertPath, cfg.cacheKeyPath)
		if err != nil {
			setupLog.Error(err, "failed to load user-provided cache server certificate")
			os.Exit(1)
		}
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
		}
	}

	// Automatic CA mode — requires POD_NAMESPACE and operator-name
	if podNamespace == "" {
		setupLog.Error(errors.New("missing required config"), "POD_NAMESPACE must be set for automatic TLS certificate management. "+
			"Alternatively, provide --cache-cert-path and --cache-key-path for manual certificate management.")
		os.Exit(1)
	}
	if cfg.operatorName == "" {
		setupLog.Error(errors.New("missing required flag"), "--operator-name is required for automatic TLS certificate management. "+
			"Alternatively, provide --cache-cert-path and --cache-key-path for manual certificate management.")
		os.Exit(1)
	}

	caCtrl := controller.NewCAController(mgr.GetClient(), podNamespace, cfg.caValidity, ctrl.Log)
	if err := mgr.Add(caCtrl); err != nil {
		setupLog.Error(err, "unable to add CA controller to manager")
		os.Exit(1)
	}

	serviceFQDN := fmt.Sprintf("%s.%s.svc.cluster.local", cfg.operatorName, podNamespace)
	dnsNames := []string{serviceFQDN, cfg.operatorName, "localhost"}

	certMgr := controller.NewCertManager(caCtrl, cfg.serverCertValidity, dnsNames, ctrl.Log)
	if err := mgr.Add(certMgr); err != nil {
		setupLog.Error(err, "unable to add certificate manager to manager")
		os.Exit(1)
	}

	return certMgr.TLSConfig()
}

func setupCacheServer(mgr ctrl.Manager, cfg config, tlsConfig *tls.Config) *cache.RuleSetCache {
	rulesetCache := cache.NewRuleSetCache()
	gcConfig := &cache.GarbageCollectionConfig{
		GCInterval: cfg.cacheGCInterval,
		MaxAge:     cfg.cacheMaxAge,
		MaxSize:    cfg.cacheMaxSize,
	}
	cacheServer := cache.NewServer(rulesetCache, fmt.Sprintf(":%d", cfg.cacheServerPort), ctrl.Log, gcConfig)
	cacheServer.SetTLSConfig(tlsConfig)
	if err := mgr.Add(cacheServer); err != nil {
		setupLog.Error(err, "unable to add cache server to manager")
		os.Exit(1)
	}
	return rulesetCache
}

func setupIstioPrerequisites(mgr ctrl.Manager, cfg config, podNamespace string) {
	if cfg.operatorName == "" || podNamespace == "" {
		setupLog.Info("Skipping Istio prerequisites: --operator-name and/or POD_NAMESPACE not set")
		return
	}

	istioPrereqs := controller.NewIstioPrerequisites(mgr.GetClient(), mgr.GetAPIReader(), cfg.operatorName, podNamespace, cfg.istioRevision)
	if err := mgr.Add(istioPrereqs); err != nil {
		setupLog.Error(err, "unable to add Istio prerequisites runnable to manager")
		os.Exit(1)
	}
}

func setupHealthChecks(mgr ctrl.Manager) {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

func validateFlags(cfg config) {
	if cfg.envoyClusterName == "" {
		setupLog.Error(errors.New("missing required flag"), "envoy-cluster-name is required")
		os.Exit(1)
	}

	if cfg.caValidity < pki.MinCAValidity {
		setupLog.Error(errors.New("invalid flag value"), fmt.Sprintf("ca-validity must be at least %v, got %v", pki.MinCAValidity, cfg.caValidity))
		os.Exit(1)
	}

	if cfg.serverCertValidity < pki.MinServerCertValidity {
		setupLog.Error(errors.New("invalid flag value"), fmt.Sprintf("server-cert-validity must be at least %v, got %v", pki.MinServerCertValidity, cfg.serverCertValidity))
		os.Exit(1)
	}

	if (cfg.cacheCertPath == "") != (cfg.cacheKeyPath == "") {
		setupLog.Error(errors.New("invalid flag combination"), "--cache-cert-path and --cache-key-path must both be set or both be empty")
		os.Exit(1)
	}
}
