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
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/controller"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/defaults"
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
	setupLog = ctrl.Log.WithName("setup")
	logFlags()
	validateFlags(cfg)

	tlsOpts := buildTLSOpts()

	podNamespace := os.Getenv("POD_NAMESPACE")
	if podNamespace == "" {
		setupLog.Error(errors.New("missing required environment variable"), "POD_NAMESPACE must be set (typically via the downward API)")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                buildMetricsServerOptions(cfg, tlsOpts),
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.enableLeaderElect,
		LeaderElectionID:       "waf.k8s.coraza.io",
		Cache:                  buildCacheOptions(podNamespace),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	kubeClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create kubernetes clientset")
		os.Exit(1)
	}

	rulesetCache := setupCacheServer(mgr, cfg, kubeClient)
	setupIstioPrerequisites(mgr, cfg, podNamespace)

	if err := controller.SetupControllers(mgr, rulesetCache, cfg.envoyClusterName, cfg.istioRevision, cfg.defaultWasmImage, podNamespace, kubeClient); err != nil {
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
	metricsCertPath   string
	metricsCertName   string
	metricsCertKey    string
	cacheGCInterval   time.Duration
	cacheMaxAge       time.Duration
	cacheMaxSize      int
	cacheServerPort   int
	envoyClusterName  string
	istioRevision     string
	defaultWasmImage  string
	operatorName      string
}

func parseFlags() config {
	var cfg config

	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or leave as 0 to disable the metrics service.")
	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&cfg.enableLeaderElect, "leader-elect", false, "Enable leader election for controller manager. "+
		"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&cfg.metricsCertPath, "metrics-cert-path", "", "The directory that contains the metrics server certificate.")
	flag.StringVar(&cfg.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&cfg.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.DurationVar(&cfg.cacheGCInterval, "cache-gc-interval", cache.CacheGCInterval, "How often to check for and remove stale cache entries in the RuleSet cache")
	flag.DurationVar(&cfg.cacheMaxAge, "cache-max-age", cache.CacheMaxAge, "Maximum age of a cache entry before it's considered stale in the RuleSet cache")
	flag.IntVar(&cfg.cacheMaxSize, "cache-max-size", cache.CacheMaxSize, fmt.Sprintf("Maximum total size of all cached rules in the RuleSet cache in bytes (default %dMB)", cache.CacheMaxSize/(1024*1024)))
	flag.IntVar(&cfg.cacheServerPort, "cache-server-port", controller.DefaultRuleSetCacheServerPort, fmt.Sprintf("Port number for the RuleSet cache server to listen on (default %d)", controller.DefaultRuleSetCacheServerPort))
	flag.StringVar(&cfg.envoyClusterName, "envoy-cluster-name", "", "The Envoy cluster name pointing to the RuleSet cache server (required)")
	flag.StringVar(&cfg.istioRevision, "istio-revision", "", "The Istio revision label value for managed Istio resources")
	flag.StringVar(&cfg.defaultWasmImage, "default-wasm-image", resolveDefaultWasmImage(),
		"Default OCI reference for the Coraza WASM plugin when an Engine omits spec.driver.istio.wasm.image")
	flag.StringVar(&cfg.operatorName, "operator-name", "", "The operator release name used to derive managed resource names (when unset, Istio prerequisites are skipped)")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	return cfg
}

func resolveDefaultWasmImage() string {
	if v := os.Getenv("CORAZA_DEFAULT_WASM_IMAGE"); v != "" {
		return v
	}
	return defaults.DefaultCorazaWasmOCIReference
}

func logFlags() {
	var kvs []any
	flag.VisitAll(func(f *flag.Flag) {
		kvs = append(kvs, f.Name, f.Value.String())
	})
	setupLog.Info("configuration", kvs...)
}

func buildTLSOpts() []func(*tls.Config) {
	return []func(*tls.Config){
		func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS13
			// Disable HTTP/2 to mitigate HTTP/2 Rapid Reset (CVE-2023-44487)
			// and related stream-cancellation DoS attacks.
			c.NextProtos = []string{"http/1.1"}
		},
	}
}

func buildMetricsServerOptions(cfg config, tlsOpts []func(*tls.Config)) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:    cfg.metricsAddr,
		SecureServing:  true,
		TLSOpts:        tlsOpts,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
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

// buildCacheOptions returns cache options that scope the NetworkPolicy informer
// to the operator namespace. Without this, the controller would require
// cluster-wide list/watch on NetworkPolicies.
func buildCacheOptions(operatorNamespace string) ctrlcache.Options {
	return ctrlcache.Options{
		DefaultTransform: ctrlcache.TransformStripManagedFields(),
		ByObject: map[client.Object]ctrlcache.ByObject{
			&networkingv1.NetworkPolicy{}: {
				Namespaces: map[string]ctrlcache.Config{
					operatorNamespace: {},
				},
			},
		},
	}
}

func setupCacheServer(mgr ctrl.Manager, cfg config, kubeClient *kubernetes.Clientset) *cache.RuleSetCache {
	rulesetCache := cache.NewRuleSetCache()
	gcConfig := &cache.GarbageCollectionConfig{
		GCInterval: cfg.cacheGCInterval,
		MaxAge:     cfg.cacheMaxAge,
		MaxSize:    cfg.cacheMaxSize,
	}
	tokenReview := kubeClient.AuthenticationV1().TokenReviews()
	cacheServer := cache.NewServer(rulesetCache, fmt.Sprintf(":%d", cfg.cacheServerPort), ctrl.Log, gcConfig, tokenReview)
	if err := mgr.Add(cacheServer); err != nil {
		setupLog.Error(err, "unable to add cache server to manager")
		os.Exit(1)
	}
	return rulesetCache
}

func setupIstioPrerequisites(mgr ctrl.Manager, cfg config, podNamespace string) {
	if cfg.operatorName == "" {
		setupLog.Info("Skipping Istio prerequisites: --operator-name not set")
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

func validateDefaultWasmImage(ref string) error {
	if ref == "" {
		return errors.New("must be non-empty")
	}
	if len(ref) > wafv1alpha1.MaxImageLen {
		return fmt.Errorf("must be at most %d characters (got %d)", wafv1alpha1.MaxImageLen, len(ref))
	}
	if !strings.HasPrefix(ref, "oci://") {
		return errors.New("must be an OCI reference starting with oci://")
	}
	return nil
}

func validateFlags(cfg config) {
	if cfg.envoyClusterName == "" {
		setupLog.Error(errors.New("missing required flag"), "envoy-cluster-name is required")
		os.Exit(1)
	}
	if err := validateDefaultWasmImage(cfg.defaultWasmImage); err != nil {
		setupLog.Error(err, "invalid default-wasm-image")
		os.Exit(1)
	}
}
