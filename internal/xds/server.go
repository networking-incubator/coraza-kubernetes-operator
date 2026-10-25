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
	"errors"
	"fmt"
	"io"
	"strings"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	extensionv3 "github.com/envoyproxy/go-control-plane/envoy/service/extension/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	authorizationHeader = "authorization"
	nackLogLimit        = 256
)

// Server implements the streaming state-of-the-world ECDS service directly.
// It deliberately does not use the go-control-plane cache adapter: per-stream
// authentication and authorization must run before any snapshot lookup, and
// identity comes only from TokenReview, never from node metadata.
type Server struct {
	extensionv3.UnimplementedExtensionConfigDiscoveryServiceServer

	Store   *SnapshotStore
	Auth    *Authenticator
	Authz   *AuthzIndex
	Streams *StreamRegistry
	Limits  Limits
	Metrics *Metrics
	Log     logr.Logger
}

// Register registers the ECDS service with a gRPC server.
func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	extensionv3.RegisterExtensionConfigDiscoveryServiceServer(registrar, s)
}

// StreamExtensionConfigs serves one SotW ECDS stream.
func (s *Server) StreamExtensionConfigs(stream extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsServer) error {
	ctx := stream.Context()
	log := s.logger()

	first, err := stream.Recv()
	if err != nil {
		return receiveStatus(err)
	}

	token, err := bearerToken(ctx)
	if err != nil {
		log.Info("ECDS stream rejected", "reason", "authorization metadata", "detail", err.Error())
		s.Metrics.AuthFailure("error")
		return status.Error(codes.Unauthenticated, authFailureMessage)
	}
	identity, err := s.Auth.Authenticate(ctx, token)
	if err != nil {
		log.Info("ECDS stream rejected", "reason", "token review", "detail", err.Error())
		s.Metrics.AuthFailure("error")
		return status.Error(codes.Unauthenticated, authFailureMessage)
	}
	s.Metrics.AuthFailure("authenticated")

	resourceName, err := singleResourceName(first.GetResourceNames())
	if err != nil {
		return status.Error(codes.InvalidArgument, invalidSubscriptionMessage)
	}
	if !s.Authz.Authorize(resourceName, *identity) {
		s.Metrics.AuthFailure("unauthorized")
		return status.Error(codes.PermissionDenied, permissionDeniedMessage)
	}

	session, err := s.Streams.Register(ctx, resourceName, *identity, token)
	if err != nil {
		return status.Error(codes.ResourceExhausted, "stream limit exceeded")
	}
	defer s.Streams.Unregister(session.ID)

	state := &subscription{}
	if err := s.processRequest(session, state, first, &resourceName, *identity, log); err != nil {
		return err
	}

	requests := make(chan *discoveryv3.DiscoveryRequest, 1)
	receiveErrors := make(chan error, 1)
	go func() {
		for {
			request, err := stream.Recv()
			if err != nil {
				select {
				case receiveErrors <- err:
				case <-session.Context().Done():
				}
				return
			}
			select {
			case requests <- request:
			case <-session.Context().Done():
				return
			}
		}
	}()

	for {
		// Capture the change channel before reading the store so a snapshot
		// written in between still closes the captured channel.
		changed := s.Store.Changed()
		if snapshot, ok := s.Store.Get(resourceName); ok &&
			snapshot.Version != state.lastVersion &&
			snapshot.Version != state.nackVersion {
			if err := s.send(stream, state, snapshot); err != nil {
				return err
			}
		}

		select {
		case <-session.Context().Done():
			return streamCloseStatus(session.Context())
		case err := <-receiveErrors:
			return receiveStatus(err)
		case request := <-requests:
			if err := s.processRequest(session, state, request, &resourceName, *identity, log); err != nil {
				return err
			}
		case <-changed:
		}
	}
}

// processRequest handles one ACK, NACK, or (re)subscription.
func (s *Server) processRequest(
	session *StreamSession,
	state *subscription,
	request *discoveryv3.DiscoveryRequest,
	resourceName *string,
	identity Identity,
	log logr.Logger,
) error {
	switch state.classify(request) {
	case requestAck:
		state.lastVersion = state.acknowledgedVersion(request)
		s.Metrics.Ack()
		return nil

	case requestNack:
		version := state.acknowledgedVersion(request)
		state.nackVersion = version
		s.Metrics.Nack("rejected")
		log.V(1).Info("ECDS response rejected by client",
			"namespace", identity.Namespace, "serviceAccount", identity.Name, "version", version,
			"detail", boundedDetail(request.GetErrorDetail().GetMessage()))
		return nil

	default:
		name, err := singleResourceName(request.GetResourceNames())
		if err != nil {
			return status.Error(codes.InvalidArgument, invalidSubscriptionMessage)
		}
		if name != *resourceName {
			if !s.Authz.Authorize(name, identity) {
				s.Metrics.AuthFailure("unauthorized")
				return status.Error(codes.PermissionDenied, permissionDeniedMessage)
			}
			*resourceName = name
			s.Streams.SetResource(session.ID, name)
		}
		// A new or updated subscription expects a response even when the
		// snapshot version is unchanged.
		state.lastVersion = ""
		state.nackVersion = ""
		return nil
	}
}

func (s *Server) send(
	stream extensionv3.ExtensionConfigDiscoveryService_StreamExtensionConfigsServer,
	state *subscription,
	snapshot *Snapshot,
) error {
	resource, err := anypb.New(snapshot.Resource)
	if err != nil {
		return status.Error(codes.Internal, "failed to encode ECDS resource")
	}
	response := &discoveryv3.DiscoveryResponse{
		VersionInfo: snapshot.Version,
		Resources:   []*anypb.Any{resource},
		TypeUrl:     TypeURL,
		Nonce:       state.nextNonce(),
	}
	if err := stream.Send(response); err != nil {
		return err
	}
	state.lastVersion = snapshot.Version
	return nil
}

func (s *Server) logger() logr.Logger {
	if s.Log.GetSink() == nil {
		return logr.Discard()
	}
	return s.Log
}

// bearerToken extracts exactly one "Bearer <token>" authorization header.
func bearerToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", errors.New("missing gRPC metadata")
	}
	values := md.Get(authorizationHeader)
	if len(values) != 1 {
		return "", fmt.Errorf("missing or duplicate authorization header (keys: %v)", metadataKeys(md))
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, bearerScheme) || strings.TrimSpace(token) == "" {
		return "", errors.New("malformed authorization header")
	}
	return strings.TrimSpace(token), nil
}

// metadataKeys returns the incoming metadata keys without their values.
func metadataKeys(md metadata.MD) []string {
	keys := make([]string, 0, len(md))
	for key := range md {
		keys = append(keys, key)
	}
	return keys
}

func receiveStatus(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// streamCloseStatus maps a bounded close cause to a generic gRPC status.
func streamCloseStatus(ctx context.Context) error {
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, ErrRevoked):
		return status.Error(codes.PermissionDenied, permissionDeniedMessage)
	case errors.Is(cause, ErrRevalidateFailed):
		return status.Error(codes.Unauthenticated, authFailureMessage)
	case errors.Is(cause, ErrLifetimeExpired), errors.Is(cause, ErrServerShutdown):
		return status.Error(codes.Unavailable, "stream must be re-established")
	default:
		return nil
	}
}

func boundedDetail(detail string) string {
	if len(detail) > nackLogLimit {
		return detail[:nackLogLimit]
	}
	return detail
}
