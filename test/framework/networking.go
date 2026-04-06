package framework

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// -----------------------------------------------------------------------------
// Ports
// -----------------------------------------------------------------------------

// AllocatePort returns the next available local port for port forwarding.
func AllocatePort() string {
	port := atomic.AddUint32(&portCounter, 1) - 1
	return fmt.Sprintf("%d", port)
}

// -----------------------------------------------------------------------------
// Gateway
// -----------------------------------------------------------------------------

// GatewayProxy manages a port-forward to a Gateway and provides HTTP
// assertion helpers for testing WAF behavior.
type GatewayProxy struct {
	s         *Scenario
	namespace string
	gateway   string
	localPort string
	baseURL   string
	httpc     *http.Client
	cancel    context.CancelFunc
}

// ProxyToGateway sets up a SPDY port-forward to the named Gateway's pod
// and returns a GatewayProxy for making HTTP requests. The port-forward is
// automatically cleaned up when the scenario ends.
func (s *Scenario) ProxyToGateway(namespace, gatewayName string) *GatewayProxy {
	s.T.Helper()
	port := AllocatePort()
	ctx, cancel := context.WithCancel(context.Background())

	proxy := &GatewayProxy{
		s:         s,
		namespace: namespace,
		gateway:   gatewayName,
		localPort: port,
		baseURL:   fmt.Sprintf("http://localhost:%s", port),
		httpc:     &http.Client{Timeout: 10 * time.Second},
		cancel:    cancel,
	}

	// Wait for the gateway pod to be Ready before starting port-forward.
	s.waitForGatewayPodReady(namespace, gatewayName)

	go proxy.maintain(ctx)

	// Wait for the port-forward to accept connections.
	require.Eventually(s.T, func() bool {
		resp, err := proxy.httpc.Get(proxy.baseURL)
		if err != nil {
			return false
		}
		defer func() {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
		return true
	}, DefaultTimeout, time.Second,
		"port-forward to %s/%s (localhost:%s) not ready", namespace, gatewayName, port,
	)

	s.OnCleanup(func() {
		cancel()
	})

	s.T.Logf("Port-forwarding %s/%s -> localhost:%s", namespace, gatewayName, port)
	return proxy
}

// URL returns the full URL for a given path through the proxy.
func (g *GatewayProxy) URL(path string) string {
	return g.baseURL + path
}

// Get makes a GET request through the proxy and returns the result.
func (g *GatewayProxy) Get(path string) *HTTPResult {
	resp, err := g.httpc.Get(g.URL(path))
	if err != nil {
		return &HTTPResult{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return &HTTPResult{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       body,
	}
}

// ExpectBlocked polls until the given path returns HTTP 403 (blocked by WAF).
func (g *GatewayProxy) ExpectBlocked(path string) {
	g.s.T.Helper()
	g.ExpectStatus(path, http.StatusForbidden)
}

// ExpectAllowed polls until the given path returns HTTP 200, confirming
// the request passed through the WAF and reached the backend. This requires
// an HTTPRoute and echo backend to be deployed (see CreateEchoBackend and
// CreateHTTPRoute). Checking for 200 rather than "not 403" avoids ambiguity:
// a 404 without a backend doesn't prove the WAF allowed the request — it
// could also mean the route is misconfigured.
func (g *GatewayProxy) ExpectAllowed(path string) {
	g.s.T.Helper()
	require.EventuallyWithT(g.s.T, func(collect *assert.CollectT) {
		resp, err := g.httpc.Get(g.URL(path))
		if !assert.NoError(collect, err) {
			return
		}
		defer func() {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
		assert.Equal(collect, http.StatusOK, resp.StatusCode,
			"expected %s to be allowed (200), got: %d", path, resp.StatusCode)
	}, DefaultTimeout, DefaultInterval)
}

// ExpectStatus polls until the given path returns the expected HTTP status.
func (g *GatewayProxy) ExpectStatus(path string, code int) {
	g.s.T.Helper()
	require.EventuallyWithT(g.s.T, func(collect *assert.CollectT) {
		resp, err := g.httpc.Get(g.URL(path))
		if !assert.NoError(collect, err) {
			return
		}
		defer func() {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
		assert.Equal(collect, code, resp.StatusCode,
			"expected %s to return %d, got: %d", path, code, resp.StatusCode)
	}, DefaultTimeout, DefaultInterval)
}

// HTTPResult holds the result of an HTTP request.
type HTTPResult struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Err        error
}

// Post makes a POST request with the given body through the proxy.
func (g *GatewayProxy) Post(path, contentType, body string) *HTTPResult {
	resp, err := g.httpc.Post(g.URL(path), contentType, strings.NewReader(body))
	if err != nil {
		return &HTTPResult{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	return &HTTPResult{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       respBody,
	}
}

// DoRequest makes a custom HTTP request through the proxy.
func (g *GatewayProxy) DoRequest(req *http.Request) *HTTPResult {
	resp, err := g.httpc.Do(req)
	if err != nil {
		return &HTTPResult{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return &HTTPResult{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       body,
	}
}

// GetWithHeaders makes a GET request with custom headers through the proxy.
func (g *GatewayProxy) GetWithHeaders(path string, headers map[string]string) *HTTPResult {
	req, err := http.NewRequest(http.MethodGet, g.URL(path), nil)
	if err != nil {
		return &HTTPResult{Err: err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return g.DoRequest(req)
}

// PostWithHeaders makes a POST request with custom headers through the proxy.
func (g *GatewayProxy) PostWithHeaders(path, contentType, body string, headers map[string]string) *HTTPResult {
	req, err := http.NewRequest(http.MethodPost, g.URL(path), strings.NewReader(body))
	if err != nil {
		return &HTTPResult{Err: err}
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return g.DoRequest(req)
}

// ExpectPostBlocked polls until a POST request returns HTTP 403.
func (g *GatewayProxy) ExpectPostBlocked(path, contentType, body string) {
	g.s.T.Helper()
	g.ExpectPostStatus(path, contentType, body, http.StatusForbidden)
}

// ExpectPostAllowed polls until a POST request returns HTTP 200.
func (g *GatewayProxy) ExpectPostAllowed(path, contentType, body string) {
	g.s.T.Helper()
	g.ExpectPostStatus(path, contentType, body, http.StatusOK)
}

// ExpectPostStatus polls until a POST request returns the expected status.
func (g *GatewayProxy) ExpectPostStatus(path, contentType, body string, code int) {
	g.s.T.Helper()
	require.EventuallyWithT(g.s.T, func(collect *assert.CollectT) {
		resp, err := g.httpc.Post(g.URL(path), contentType, strings.NewReader(body))
		if !assert.NoError(collect, err) {
			return
		}
		defer func() {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
		assert.Equal(collect, code, resp.StatusCode,
			"expected POST %s to return %d, got: %d", path, code, resp.StatusCode)
	}, DefaultTimeout, DefaultInterval)
}

// ExpectHeaderBlocked polls until a GET with custom headers returns HTTP 403.
func (g *GatewayProxy) ExpectHeaderBlocked(path string, headers map[string]string) {
	g.s.T.Helper()
	g.ExpectHeaderStatus(path, headers, http.StatusForbidden)
}

// ExpectHeaderAllowed polls until a GET with custom headers returns HTTP 200.
func (g *GatewayProxy) ExpectHeaderAllowed(path string, headers map[string]string) {
	g.s.T.Helper()
	g.ExpectHeaderStatus(path, headers, http.StatusOK)
}

// ExpectHeaderStatus polls until a GET with custom headers returns the expected status.
func (g *GatewayProxy) ExpectHeaderStatus(path string, headers map[string]string, code int) {
	g.s.T.Helper()
	require.EventuallyWithT(g.s.T, func(collect *assert.CollectT) {
		req, err := http.NewRequest(http.MethodGet, g.URL(path), nil)
		if !assert.NoError(collect, err) {
			return
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := g.httpc.Do(req)
		if !assert.NoError(collect, err) {
			return
		}
		defer func() {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
		assert.Equal(collect, code, resp.StatusCode,
			"expected GET %s with headers to return %d, got: %d", path, code, resp.StatusCode)
	}, DefaultTimeout, DefaultInterval)
}

// -----------------------------------------------------------------------------
// Gateway - Port Forward Management
// -----------------------------------------------------------------------------

// logf logs via t.Logf if the test is still running. The maintain goroutine
// may outlive the test, and t.Logf panics after the test finishes (Go 1.24+).
// t.Context() is cancelled when the test completes, so we check it first.
func (g *GatewayProxy) logf(format string, args ...any) {
	if g.s.T.Context().Err() != nil {
		return
	}
	g.s.T.Logf(format, args...)
}

func (g *GatewayProxy) maintain(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 10 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		start := time.Now()
		err := g.runPortForward(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			g.logf("port-forward %s/%s restarting (backoff %s): %v",
				g.namespace, g.gateway, backoff, err)
		}

		if time.Since(start) > maxBackoff {
			backoff = time.Second
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, maxBackoff)
	}
}

func (g *GatewayProxy) runPortForward(ctx context.Context) error {
	labelSelector := fmt.Sprintf(
		"gateway.networking.k8s.io/gateway-name=%s", g.gateway,
	)

	pods, err := g.s.F.KubeClient.CoreV1().Pods(g.namespace).List(
		ctx,
		metav1.ListOptions{LabelSelector: labelSelector},
	)
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no pods matching %s", labelSelector)
	}

	podName := pods.Items[0].Name

	transport, upgrader, err := spdy.RoundTripperFor(g.s.F.RestConfig)
	if err != nil {
		return fmt.Errorf("create SPDY transport: %w", err)
	}

	pfURL := g.s.F.KubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(g.namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", pfURL)

	// Bridge context cancellation to the port-forwarder's stopCh.
	// The done channel ensures the bridge goroutine exits when
	// ForwardPorts returns, rather than leaking until ctx is cancelled.
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			close(stopCh)
		case <-done:
		}
	}()

	pf, err := portforward.New(dialer,
		[]string{fmt.Sprintf("%s:80", g.localPort)},
		stopCh, nil, io.Discard, io.Discard,
	)
	if err != nil {
		close(done)
		return fmt.Errorf("create port-forwarder: %w", err)
	}

	err = pf.ForwardPorts()
	close(done)
	return err
}
