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
	"errors"
	"strconv"
	"strings"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
)

const (
	invalidSubscriptionMessage = "one ECDS resource name is required"
	permissionDeniedMessage    = "subscription is not authorized"
	authFailureMessage         = "authentication failed"
)

var errInvalidSubscription = errors.New(invalidSubscriptionMessage)

// requestKind classifies a received DiscoveryRequest.
type requestKind int

const (
	requestSubscribed requestKind = iota
	requestAck
	requestNack
)

// subscription is the per-stream nonce and version state machine.
type subscription struct {
	nonce       uint64
	lastNonce   string
	lastVersion string
	nackVersion string
}

// nextNonce returns the next per-stream monotonic nonce.
func (s *subscription) nextNonce() string {
	s.nonce++
	s.lastNonce = strconv.FormatUint(s.nonce, 10)
	return s.lastNonce
}

// classify distinguishes an ACK/NACK of the last response from a new or
// updated subscription.
func (s *subscription) classify(request *discoveryv3.DiscoveryRequest) requestKind {
	if request.GetResponseNonce() == "" || request.GetResponseNonce() != s.lastNonce {
		return requestSubscribed
	}
	if request.GetErrorDetail() != nil {
		return requestNack
	}
	return requestAck
}

// acknowledgedVersion returns the version an ACK/NACK refers to, falling back
// to the last version sent when the client omits version_info.
func (s *subscription) acknowledgedVersion(request *discoveryv3.DiscoveryRequest) string {
	if version := request.GetVersionInfo(); version != "" {
		return version
	}
	return s.lastVersion
}

// singleResourceName validates the SotW profile: exactly one known-format
// resource name per subscription. Wildcards and multi-resource subscriptions
// are rejected.
func singleResourceName(names []string) (string, error) {
	if len(names) != 1 {
		return "", errInvalidSubscription
	}
	name := names[0]
	if name == "" || !strings.HasPrefix(name, resourceNamePrefix) || strings.ContainsAny(name, "*?") {
		return "", errInvalidSubscription
	}
	return name, nil
}
