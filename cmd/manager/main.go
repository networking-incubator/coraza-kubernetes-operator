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
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/controller"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/xds"
	// +kubebuilder:scaffold:imports
)

// -----------------------------------------------------------------------------
// Scheme Registration
// -----------------------------------------------------------------------------

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(wafv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	// +kubebuilder:scaffold:scheme
}

// -----------------------------------------------------------------------------
// Vars
// -----------------------------------------------------------------------------

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// servingCertBootstrapTimeout bounds the API calls that provision the xDS
// serving Secret. They run before the manager starts, on a client with no
// per-request timeout, so an unreachable API server would otherwise hang
// startup rather than fail it.
const servingCertBootstrapTimeout = 2 * time.Minute

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func main() {
	cfg := parseFlags()
	setupLog = ctrl.Log.WithName("setup")
	logFlags()
	setMemoryLimit()
	setupLog.Info("Deprecated flags are accepted but ignored",
		"flags", "--default-wasm-image, --envoy-cluster-name")

	rulePolicy, err := cfg.rulePolicy()
	if err != nil {
		setupLog.Error(err, "Invalid cluster rule policy")
		os.Exit(1)
	}

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

	if err := bootstrapServingCert(cfg, podNamespace); err != nil {
		setupLog.Error(err, "unable to provision the xDS serving certificate")
		os.Exit(1)
	}

	rulesetCache := setupRuleSetCache(cfg)

	xdsDeps, err := xds.NewDependencies(xds.Config{
		KubeClient:    kubeClient,
		Reader:        mgr.GetClient(),
		APIReader:     mgr.GetAPIReader(),
		Cache:         mgr.GetCache(),
		BindAddress:   cfg.xdsBindAddress,
		TokenAudience: cfg.xdsTokenAudience,
		CertDir:       cfg.xdsCertDir,
		CertName:      cfg.xdsCertName,
		KeyName:       cfg.xdsKeyName,
		CAPath:        cfg.xdsCAPath,
		Limits:        cfg.xdsLimits,
		Registry:      metrics.Registry,
		Logger:        ctrl.Log.WithName("xds"),
	})
	if err != nil {
		setupLog.Error(err, "unable to construct xDS serving plane")
		os.Exit(1)
	}

	ecdsConfig, assetBaseURL, err := buildECDSConfig(cfg, podNamespace, xdsDeps.CA)
	if err != nil {
		setupLog.Error(err, "invalid ECDS configuration")
		os.Exit(1)
	}
	if err := controller.SetupControllers(mgr, controller.Deps{
		KubeClient:        kubeClient,
		RuleSetCache:      rulesetCache,
		IstioRevision:     cfg.istioRevision,
		OperatorNamespace: podNamespace,
		ECDSConfig:        ecdsConfig,
		AssetBaseURL:      assetBaseURL,
		XDS:               xdsDeps,
		RulePolicy:        rulePolicy,
		MaxRules:          cfg.ruleMaxRules,
	}); err != nil {
		setupLog.Error(err, "unable to setup controllers")
		os.Exit(1)
	}

	if err := mgr.Add(xdsDeps.Listener); err != nil {
		setupLog.Error(err, "unable to add xDS listener runnable")
		os.Exit(1)
	}
	if err := mgr.Add(xdsDeps.Streams); err != nil {
		setupLog.Error(err, "unable to add xDS revalidation runnable")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	setupHealthChecks(mgr, xdsDeps)

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

	// Deprecated and ignored: kept so the chart can keep passing them until
	// Phase 7 removes the flags and the chart arguments together.
	envoyClusterName string
	defaultWasmImage string

	istioRevision string
	operatorName  string

	// ECDS settings: the address Gateways dial for config discovery, the port
	// appended when the address has none, the base URL of the WASM asset, and
	// the CA used to verify the operator certificate.
	xdsAdvertiseAddress string
	xdsPort             int
	assetBaseURL        string
	xdsCAPath           string
	operatorServiceName string

	// ECDS serving plane.
	xdsBindAddress   string
	xdsTokenAudience string
	xdsCertDir       string
	xdsCertName      string
	xdsKeyName       string
	xdsCertSecret    string
	xdsCertDNSNames  []string
	xdsLimits        xds.Limits

	// Cluster rule policy, applied to RuleSet content not accepted yet.
	ruleDeniedOperators     string
	ruleDeniedDirectives    string
	ruleMaxRequestBodyLimit int64
	ruleResponseBodyAccess  string
	ruleMaxRules            int
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
	// Deprecated: the RuleSet cache server is not started and nothing populates
	// the cache, so these four flags are accepted for chart compatibility only.
	flag.DurationVar(&cfg.cacheGCInterval, "cache-gc-interval", cache.CacheGCInterval, "Deprecated and ignored")
	flag.DurationVar(&cfg.cacheMaxAge, "cache-max-age", cache.CacheMaxAge, "Deprecated and ignored")
	flag.IntVar(&cfg.cacheMaxSize, "cache-max-size", cache.CacheMaxSize, "Deprecated and ignored")
	flag.IntVar(&cfg.cacheServerPort, "cache-server-port", controller.DefaultRuleSetCacheServerPort, "Deprecated and ignored")
	// Deprecated: accepted for chart compatibility until Phase 7 removes both
	// the flags and the chart arguments. Neither value is used.
	flag.StringVar(&cfg.envoyClusterName, "envoy-cluster-name", "", "Deprecated and ignored")
	flag.StringVar(&cfg.istioRevision, "istio-revision", "", "The Istio revision label value for managed Istio resources")
	flag.StringVar(&cfg.defaultWasmImage, "default-wasm-image", "", "Deprecated and ignored")
	flag.StringVar(&cfg.operatorName, "operator-name", "",
		"Helm release name; the fallback Service name for the default xDS advertise address when --operator-service-name is unset")
	flag.StringVar(&cfg.xdsAdvertiseAddress, "xds-advertise-address", "",
		"The operator address Gateways dial for ECDS; defaults to <operator-service-name>.<POD_NAMESPACE>.svc.cluster.local")
	flag.IntVar(&cfg.xdsPort, "xds-port", controller.DefaultRuleSetCacheServerPort,
		"The operator ECDS port appended to the advertise address when it has no port")
	flag.StringVar(&cfg.assetBaseURL, "asset-base-url", "",
		"The base URL Envoy fetches the WASM asset from; defaults to https://<advertise-address>")
	flag.StringVar(&cfg.xdsCAPath, "xds-ca-path", "/etc/coraza-tls/ca.crt",
		"The CA bundle Gateways use to verify the operator certificate")
	flag.StringVar(&cfg.operatorServiceName, "operator-service-name", "",
		"The operator Service name used to derive the default xDS advertise address")
	flag.StringVar(&cfg.xdsBindAddress, "xds-bind-address", ":18080",
		"The address the ECDS and WASM asset listener binds to")
	flag.StringVar(&cfg.xdsTokenAudience, "xds-token-audience", "",
		"The expected TokenReview audience; empty uses the API server default audience")
	flag.StringVar(&cfg.xdsCertDir, "xds-cert-path", "/etc/coraza-tls",
		"The directory that contains the xDS serving certificate and CA bundle")
	flag.StringVar(&cfg.xdsCertName, "xds-cert-name", "tls.crt",
		"The name of the xDS serving certificate file")
	flag.StringVar(&cfg.xdsKeyName, "xds-key-name", "tls.key",
		"The name of the xDS serving key file")
	flag.StringVar(&cfg.xdsCertSecret, "xds-cert-secret", "",
		"The Secret in POD_NAMESPACE holding the xDS serving certificate. When set, the operator "+
			"creates it with a self-signed CA if it is missing or unusable. Leave empty when the "+
			"certificate is provisioned externally (Helm, cert-manager).")
	flag.Func("xds-cert-dns-name",
		"An extra DNS SAN for a certificate created by --xds-cert-secret. Repeatable.",
		func(value string) error {
			value = strings.TrimSpace(value)
			if value == "" {
				return errors.New("the DNS name is empty")
			}
			cfg.xdsCertDNSNames = append(cfg.xdsCertDNSNames, value)
			return nil
		})
	cfg.xdsLimits = xds.DefaultLimits()
	cfg.xdsLimits.AddFlags(flag.CommandLine)
	addRulePolicyFlags(flag.CommandLine, &cfg)

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	return cfg
}

// addRulePolicyFlags registers the cluster rule policy flags.
func addRulePolicyFlags(fs *flag.FlagSet, cfg *config) {
	fs.StringVar(&cfg.ruleDeniedOperators, "rule-denied-operators", "",
		"Comma-separated operator names tenants may not use in new or changed rules, matched case-insensitively")
	fs.StringVar(&cfg.ruleDeniedDirectives, "rule-denied-directives", "",
		"Comma-separated directive names tenants may not use in new or changed rules, and the ctl actions with the same effect, matched case-insensitively")
	fs.Int64Var(&cfg.ruleMaxRequestBodyLimit, "rule-max-request-body-limit", validation.MaxRequestBodyLimit,
		"Highest SecRequestBodyLimit accepted in new or changed rules, in bytes; at most the envoy-coraza ceiling of 16777216")
	fs.StringVar(&cfg.ruleResponseBodyAccess, "rule-response-body-access", validation.ResponseBodyAccessAllowed,
		"Allowed or Denied; Denied rejects SecResponseBodyAccess On in new or changed rules")
	fs.IntVar(&cfg.ruleMaxRules, "rule-max-rules", controller.DefaultMaxRules,
		"Most rules one RuleSet may hold: SecRule (each rule of a chain), SecAction and SecMarker, once per Include of a file; the OWASP CRS holds about 700")
}

// rulePolicy builds the cluster rule policy from the flags.
func (cfg config) rulePolicy() (validation.RulePolicy, error) {
	if cfg.ruleMaxRules < 1 {
		return validation.RulePolicy{}, fmt.Errorf("rule-max-rules %d must be positive", cfg.ruleMaxRules)
	}
	return validation.ParseRulePolicy(cfg.ruleDeniedOperators, cfg.ruleDeniedDirectives,
		cfg.ruleMaxRequestBodyLimit, cfg.ruleResponseBodyAccess)
}

// bootstrapServingCert provisions the xDS serving Secret when --xds-cert-secret
// names one. Installs that cannot run Helm (OLM bundles, plain manifests,
// GitOps renderers) have nothing else to mint it, and the listener would never
// obtain a certificate.
//
// It runs before the manager starts and uses an uncached client: the Secret is
// needed before the cache is running, and all replicas converge on whichever
// one wins the create. The deadline keeps a stalled API server from hanging
// startup indefinitely, since this client has no per-request timeout.
func bootstrapServingCert(cfg config, podNamespace string) error {
	if cfg.xdsCertSecret == "" {
		return nil
	}

	serviceName := operatorServiceName(cfg)
	if serviceName == "" {
		return errors.New("the operator Service name is not configured: set --operator-service-name or --operator-name")
	}

	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("creating the bootstrap client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), servingCertBootstrapTimeout)
	defer cancel()

	result, err := xds.EnsureServingCert(ctx, c, xds.ServingCertOptions{
		Namespace:     podNamespace,
		Name:          cfg.xdsCertSecret,
		ServiceName:   serviceName,
		ExtraDNSNames: cfg.xdsCertDNSNames,
		CertName:      cfg.xdsCertName,
		KeyName:       cfg.xdsKeyName,
		Logger:        setupLog,
	})
	if err != nil {
		return err
	}

	if !result.Replaced {
		return nil
	}
	// Existing material was overwritten, so the pod still has the previous
	// Secret projected. Serving now would publish the superseded CA to
	// Gateways while the certificate watcher picks up the new leaf, and
	// CASource caches whichever CA it reads first. Wait for the volume to
	// catch up instead.
	setupLog.Info("The xDS serving certificate was replaced; waiting for the new one to be mounted",
		"secret", cfg.xdsCertSecret)
	return xds.WaitForMountedCert(ctx, cfg.xdsCertDir, cfg.xdsCertName, result.CertPEM, setupLog)
}

// operatorServiceName resolves the Service the Gateways dial. The chart passes
// the release fullname as --operator-name, which is also the Service name.
func operatorServiceName(cfg config) string {
	if cfg.operatorServiceName != "" {
		return cfg.operatorServiceName
	}
	return cfg.operatorName
}

// buildECDSConfig derives the ECDS endpoint and asset base URL from the flags,
// and loads the CA bundle. A missing CA is not fatal: Engine reconciliation
// reports TLSNotConfigured instead of rendering an unverified chain. A missing
// advertise address is fatal: every Engine would fail to render its EnvoyFilter.
func buildECDSConfig(cfg config, podNamespace string, ca *xds.CASource) (controller.ECDSConfig, string, error) {
	advertise := cfg.xdsAdvertiseAddress
	if advertise == "" {
		if serviceName := operatorServiceName(cfg); serviceName != "" {
			advertise = fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, podNamespace)
		}
	}
	if advertise == "" {
		return controller.ECDSConfig{}, "", fmt.Errorf("the xDS advertise address is not configured: set --xds-advertise-address, --operator-service-name, or --operator-name")
	}
	if advertise != "" {
		if _, _, err := net.SplitHostPort(advertise); err != nil {
			// A bracketed IPv6 literal is already a complete host; JoinHostPort
			// would wrap it a second time.
			host := strings.TrimSuffix(strings.TrimPrefix(advertise, "["), "]")
			advertise = net.JoinHostPort(host, strconv.Itoa(cfg.xdsPort))
		}
	}

	assetBaseURL := cfg.assetBaseURL
	if assetBaseURL == "" && advertise != "" {
		assetBaseURL = "https://" + advertise
	}

	// The listener and the Engine reconciler share one source, so a CA mounted
	// after startup reaches new EnvoyFilters without a restart.
	caPEM := ca.PEM()
	if len(caPEM) == 0 {
		setupLog.Info("xDS CA bundle unavailable; Engine reconciliation will report TLSNotConfigured",
			"path", cfg.xdsCAPath)
	}

	return controller.ECDSConfig{
		AdvertiseAddress: advertise,
		ListenPort:       listenPort(cfg.xdsBindAddress),
		CAPEM:            caPEM,
		CASource:         ca,
	}, assetBaseURL, nil
}

// listenPort returns the port the ECDS listener actually binds. The Engine
// NetworkPolicy must allow this port, not the advertised default.
func listenPort(address string) int32 {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return 0
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return 0
	}
	return int32(port)
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

// setMemoryLimit derives the Go runtime memory limit from the container memory
// limit the chart passes through the downward API, so the garbage collector
// works harder near the limit instead of letting the heap grow into an
// out-of-memory kill. It bounds collection headroom only, not live heap.
// Without a limit, as in local runs, or with GOMEMLIMIT set, it does nothing.
func setMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	containerLimit := os.Getenv("MEMORY_LIMIT_BYTES")
	limit, ok := runtimeMemoryLimit(containerLimit)
	if !ok {
		return
	}
	debug.SetMemoryLimit(limit)
	setupLog.Info("Set the Go runtime memory limit", "bytes", limit, "containerLimitBytes", containerLimit)
}

// runtimeMemoryLimit returns 90% of the container memory limit in value. The
// rest is left for memory the runtime does not count, such as thread stacks.
func runtimeMemoryLimit(value string) (int64, bool) {
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	limit = limit / 10 * 9
	return limit, limit > 0
}

// stripLastApplied drops managed fields and the kubectl last-applied
// annotation, which repeats the whole spec of an applied RuleSource or
// RuleData, before the object is cached: every replica caches these objects in
// full. The operator reads neither field and writes these kinds only through
// status patches, so the stored objects keep both. A per-type transform
// replaces the default one, so it strips managed fields too.
func stripLastApplied(in any) (any, error) {
	if obj, err := meta.Accessor(in); err == nil {
		annotations := obj.GetAnnotations()
		delete(annotations, corev1.LastAppliedConfigAnnotation)
		obj.SetAnnotations(annotations)
	}
	return ctrlcache.TransformStripManagedFields()(in)
}

// buildCacheOptions returns cache options that scope the NetworkPolicy informer
// to the operator namespace, and the Pod informer to Gateway data-plane pods
// (label-selected, with the churny parts of PodStatus stripped). Without this,
// the controllers would cache every Pod and NetworkPolicy in the cluster.
// RuleSource and RuleData are cached without their last-applied copy.
func buildCacheOptions(operatorNamespace string) ctrlcache.Options {
	return ctrlcache.Options{
		DefaultTransform: ctrlcache.TransformStripManagedFields(),
		ByObject: map[client.Object]ctrlcache.ByObject{
			&wafv1alpha1.RuleSource{}: {Transform: stripLastApplied},
			&wafv1alpha1.RuleData{}:   {Transform: stripLastApplied},
			&networkingv1.NetworkPolicy{}: {
				Namespaces: map[string]ctrlcache.Config{
					operatorNamespace: {},
				},
			},
			&corev1.Pod{}: {
				Label:     xds.GatewayPodSelector(),
				Transform: xds.GatewayPodTransform,
			},
		},
	}
}

func setupRuleSetCache(cfg config) *cache.RuleSetCache {
	rulesetCache := cache.NewRuleSetCache()
	rulesetCache.SetLogger(ctrl.Log.WithName("ruleset-cache"))
	gcConfig := &cache.GarbageCollectionConfig{
		GCInterval: cfg.cacheGCInterval,
		MaxAge:     cfg.cacheMaxAge,
		MaxSize:    cfg.cacheMaxSize,
	}

	if err := cache.RegisterUSEMetrics(metrics.Registry, rulesetCache, *gcConfig); err != nil {
		setupLog.Error(err, "unable to register USE metrics for ruleset cache")
		os.Exit(1)
	}

	// The legacy HTTP rule server is intentionally not registered: ECDS is the
	// only delivery path. The package and its tests are retained.
	return rulesetCache
}

func setupHealthChecks(mgr ctrl.Manager, deps *xds.Dependencies) {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	checks := map[string]func() error{
		"xds-assets":      deps.CheckAssets,
		"xds-server":      deps.CheckServer,
		"xds-publication": deps.CheckPublication,
	}
	for name, check := range checks {
		if err := mgr.AddReadyzCheck(name, func(_ *http.Request) error { return check() }); err != nil {
			setupLog.Error(err, "unable to set up xDS readiness check", "check", name)
			os.Exit(1)
		}
	}
}
