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
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

type listenerFixture struct {
	listener *Listener
	server   *Server
	cancel   context.CancelFunc
	pool     *x509.CertPool
	manifest *assets.Manifest
}

func newListenerFixture(t *testing.T) *listenerFixture {
	t.Helper()

	manifest, data, err := assets.ResolveDefault()
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	store := NewSnapshotStore()
	authz := NewAuthzIndex(nil, time.Hour)
	auth := NewAuthenticator(nil, "", time.Minute, time.Minute, time.Second, 0)
	streams := NewStreamRegistry(auth, authz, DefaultLimits(), metrics)
	server := &Server{
		Store:   store,
		Auth:    auth,
		Authz:   authz,
		Streams: streams,
		Metrics: metrics,
		Log:     logr.Discard(),
	}

	listener, err := NewListener(ListenerConfig{
		BindAddress: "127.0.0.1:0",
		CertDir:     "testdata",
		CertName:    "tls.crt",
		KeyName:     "tls.key",
		CAPath:      "testdata/ca.crt",
	}, server, NewAssetHandler(manifest, data, metrics), DefaultLimits(), logr.Discard())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- listener.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Listener.Start() error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("listener did not stop")
		}
	})

	select {
	case <-listener.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not bind")
	}

	caPEM, err := os.ReadFile("testdata/ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to load test CA")
	}

	return &listenerFixture{listener: listener, server: server, cancel: cancel, pool: pool, manifest: manifest}
}

func TestListenerALPNDispatch(t *testing.T) {
	fixture := newListenerFixture(t)
	address := fixture.listener.Addr().String()

	t.Run("h2 reaches gRPC", func(t *testing.T) {
		creds := credentials.NewTLS(&tls.Config{RootCAs: fixture.pool, ServerName: "localhost"})
		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(creds))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()

		stream, err := extensionv3.NewExtensionConfigDiscoveryServiceClient(conn).StreamExtensionConfigs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&discoveryv3.DiscoveryRequest{ResourceNames: []string{ResourceName("shop", "checkout", "uid")}}); err != nil {
			t.Fatal(err)
		}
		_, err = stream.Recv()
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("Recv() code = %v, want Unauthenticated (h2 must reach gRPC)", status.Code(err))
		}
	})

	t.Run("http/1.1 reaches assets, not /rules", func(t *testing.T) {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: fixture.pool, ServerName: "localhost"}, ForceAttemptHTTP2: false}
		client := &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}

		response, err := client.Get("https://" + address + assetPathPrefix + fixture.manifest.SHA256 + assetPathSuffix)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("asset status = %d, want 200", response.StatusCode)
		}
		if len(body) == 0 {
			t.Fatal("asset body is empty")
		}

		rules, err := client.Get("https://" + address + "/rules/shop/checkout")
		if err != nil {
			t.Fatal(err)
		}
		_ = rules.Body.Close()
		if rules.StatusCode != http.StatusNotFound {
			t.Fatalf("/rules status = %d, want 404", rules.StatusCode)
		}
	})
}

func TestListenerNegotiatesALPN(t *testing.T) {
	fixture := newListenerFixture(t)
	address := fixture.listener.Addr().String()

	for _, protocol := range []string{"h2", "http/1.1"} {
		t.Run(protocol, func(t *testing.T) {
			connection, err := tls.Dial("tcp", address, &tls.Config{
				RootCAs:    fixture.pool,
				ServerName: "localhost",
				NextProtos: []string{protocol},
				MinVersion: tls.VersionTLS13,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close() }()
			if got := connection.ConnectionState().NegotiatedProtocol; got != protocol {
				t.Fatalf("negotiated protocol = %q, want %q", got, protocol)
			}
		})
	}
}

func TestListenerReady(t *testing.T) {
	fixture := newListenerFixture(t)
	if err := fixture.listener.Ready(); err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
}
