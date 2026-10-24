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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ExtensionConfigTypeURL is the ECDS resource type served by the operator.
const ExtensionConfigTypeURL = "type.googleapis.com/envoy.config.core.v3.TypedExtensionConfig"

// -----------------------------------------------------------------------------
// ECDS Client
// -----------------------------------------------------------------------------

// ECDSClientOptions configures a direct ECDS test client.
type ECDSClientOptions struct {
	// Target is the gRPC target, for example "localhost:29000".
	Target string

	// Token is sent as a bearer token in the authorization metadata. Empty
	// sends no credentials (used to assert rejection).
	Token string

	// TLSConfig is required against the real operator listener. Nil selects
	// plaintext, which is only valid for in-process unit test servers.
	TLSConfig *tls.Config

	// NodeID is sent in DiscoveryRequest.node. The server must never use it
	// for identity; tests set it to probe spoofing.
	NodeID string
}

// ECDSClient is a minimal state-of-the-world ECDS client for integration tests.
type ECDSClient struct {
	conn   *grpc.ClientConn
	client extensionv3.ExtensionConfigDiscoveryServiceClient
	token  string
	nodeID string
}

// NewECDSClient dials the ECDS server. The connection is established lazily, so
// no context is needed here; pass one to Stream.
func NewECDSClient(opts ECDSClientOptions) (*ECDSClient, error) {
	if opts.Target == "" {
		return nil, errors.New("ECDS target is required")
	}

	var creds credentials.TransportCredentials
	if opts.TLSConfig != nil {
		creds = credentials.NewTLS(opts.TLSConfig)
	} else {
		creds = insecure.NewCredentials()
	}

	conn, err := grpc.NewClient(opts.Target,
		grpc.WithTransportCredentials(creds),
		// Tests usually dial a fresh port-forward; wait for it instead of
		// failing the first RPC.
		grpc.WithDefaultCallOptions(grpc.WaitForReady(true)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial ECDS server %s: %w", opts.Target, err)
	}

	return &ECDSClient{
		conn:   conn,
		client: extensionv3.NewExtensionConfigDiscoveryServiceClient(conn),
		token:  opts.Token,
		nodeID: opts.NodeID,
	}, nil
}

// NewECDSClientTLSConfig builds a TLS config that trusts caPEM and verifies
// serverName, matching the operator listener's TLS 1.3 minimum.
func NewECDSClientTLSConfig(caPEM []byte, serverName string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no certificates found in CA PEM")
	}
	return &tls.Config{
		RootCAs:    pool,
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	}, nil
}

// Close closes the gRPC connection.
func (c *ECDSClient) Close() error {
	return c.conn.Close()
}

// NewStream opens a stream without sending a request. Tests use it to send
// deliberately invalid subscriptions (empty, wildcard, or multiple resources).
func (c *ECDSClient) NewStream(ctx context.Context) (*ECDSStream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	if c.token != "" {
		streamCtx = metadata.AppendToOutgoingContext(streamCtx, "authorization", "Bearer "+c.token)
	}

	grpcStream, err := c.client.StreamExtensionConfigs(streamCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open ECDS stream: %w", err)
	}
	return &ECDSStream{
		stream: grpcStream,
		cancel: cancel,
		nodeID: c.nodeID,
	}, nil
}

// Stream opens a stream and sends the initial single-resource subscription.
func (c *ECDSClient) Stream(ctx context.Context, resourceName string) (*ECDSStream, error) {
	if resourceName == "" {
		return nil, errors.New("ECDS resource name is required")
	}

	s, err := c.NewStream(ctx)
	if err != nil {
		return nil, err
	}
	s.resourceName = resourceName
	if err := s.Send(s.request("", "")); err != nil {
		s.Close()
		return nil, fmt.Errorf("send initial ECDS request: %w", err)
	}
	return s, nil
}

// -----------------------------------------------------------------------------
// ECDS Stream
// -----------------------------------------------------------------------------

// ECDSStream is one ECDS stream. Close cancels the stream context.
type ECDSStream struct {
	stream       extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsClient
	cancel       context.CancelFunc
	resourceName string
	nodeID       string
}

// Close cancels the stream.
func (s *ECDSStream) Close() {
	s.cancel()
}

// Send sends a raw DiscoveryRequest.
func (s *ECDSStream) Send(req *discoveryv3.DiscoveryRequest) error {
	return s.stream.Send(req)
}

// Recv receives the next DiscoveryResponse.
func (s *ECDSStream) Recv() (*discoveryv3.DiscoveryResponse, error) {
	return s.stream.Recv()
}

// ExpectResponse receives one response and asserts the server set a nonce.
func (s *ECDSStream) ExpectResponse(t *testing.T) *discoveryv3.DiscoveryResponse {
	t.Helper()
	resp, err := s.Recv()
	require.NoError(t, err, "receive ECDS response")
	require.NotEmpty(t, resp.GetNonce(), "ECDS response must carry a nonce")
	return resp
}

// ACK acknowledges a response by echoing its version and nonce.
func (s *ECDSStream) ACK(t *testing.T, resp *discoveryv3.DiscoveryResponse) {
	t.Helper()
	require.NoError(t, s.Send(s.responseRequest(resp, nil)), "send ECDS ACK")
}

// NACK rejects a response with an error detail; Envoy keeps the prior config.
func (s *ECDSStream) NACK(t *testing.T, resp *discoveryv3.DiscoveryResponse, reason string) {
	t.Helper()
	detail := &status.Status{Code: int32(codes.InvalidArgument), Message: reason}
	require.NoError(t, s.Send(s.responseRequest(resp, detail)), "send ECDS NACK")
}

func (s *ECDSStream) request(versionInfo, nonce string) *discoveryv3.DiscoveryRequest {
	return &discoveryv3.DiscoveryRequest{
		Node:          &corev3.Node{Id: s.nodeID},
		VersionInfo:   versionInfo,
		ResponseNonce: nonce,
		ResourceNames: []string{s.resourceName},
		TypeUrl:       ExtensionConfigTypeURL,
	}
}

func (s *ECDSStream) responseRequest(
	resp *discoveryv3.DiscoveryResponse, detail *status.Status,
) *discoveryv3.DiscoveryRequest {
	req := s.request(resp.GetVersionInfo(), resp.GetNonce())
	req.ErrorDetail = detail
	return req
}

// -----------------------------------------------------------------------------
// ECDS Response Assertions
// -----------------------------------------------------------------------------

// ExpectSingleExtensionConfig asserts the response carries exactly one
// TypedExtensionConfig with the expected name and returns it.
func ExpectSingleExtensionConfig(
	t *testing.T, resp *discoveryv3.DiscoveryResponse, wantName string,
) *corev3.TypedExtensionConfig {
	t.Helper()
	require.Len(t, resp.GetResources(), 1, "ECDS response must contain exactly one resource")
	require.Equal(t, ExtensionConfigTypeURL, resp.GetResources()[0].GetTypeUrl(),
		"ECDS resource must be a TypedExtensionConfig")

	cfg := &corev3.TypedExtensionConfig{}
	require.NoError(t, resp.GetResources()[0].UnmarshalTo(cfg), "decode TypedExtensionConfig")
	require.Equal(t, wantName, cfg.GetName(), "ECDS resource name must match the subscription")
	return cfg
}
