/*
Copyright 2026 Shane Utt.

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
	"fmt"
	"strings"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EventMatch specifies criteria for matching Kubernetes events.
// Empty fields are treated as wildcards (match any value).
type EventMatch struct {
	Type   string // "Normal" or "Warning"
	Reason string
}

// GetEvents returns all events.k8s.io/v1 events in the given namespace.
func (s *Scenario) GetEvents(namespace string) []eventsv1.Event {
	s.T.Helper()
	events, err := s.F.KubeClient.EventsV1().Events(namespace).List(
		s.T.Context(), metav1.ListOptions{},
	)
	require.NoError(s.T, err, "list events in namespace %s", namespace)
	return events.Items
}

// ExpectEvent polls until at least one event matching the criteria exists in
// the namespace.
func (s *Scenario) ExpectEvent(namespace string, match EventMatch) {
	s.T.Helper()
	s.T.Logf("Waiting for %s event with reason %q in %s", match.Type, match.Reason, namespace)
	require.EventuallyWithT(s.T, func(collect *assert.CollectT) {
		events, err := s.F.KubeClient.EventsV1().Events(namespace).List(
			s.T.Context(), metav1.ListOptions{},
		)
		if !assert.NoError(collect, err) {
			return
		}
		found := false
		for _, e := range events.Items {
			if matchesEvent(e, match) {
				found = true
				break
			}
		}
		assert.True(collect, found,
			"no %s event with reason %q found in %s; existing events: [%s]",
			match.Type, match.Reason, namespace, summarizeEvents(events.Items),
		)
	}, DefaultTimeout, DefaultInterval)
}

// ExpectNoEvent asserts that no event matching the criteria currently exists
// in the namespace. This is a point-in-time check — call it after the system
// has settled (e.g., after ExpectEngineReady).
func (s *Scenario) ExpectNoEvent(namespace string, match EventMatch) {
	s.T.Helper()
	events := s.GetEvents(namespace)
	for _, e := range events {
		if matchesEvent(e, match) {
			s.T.Errorf("unexpected %s event with reason %q in %s: %s",
				e.Type, e.Reason, namespace, e.Note)
		}
	}
}

func matchesEvent(e eventsv1.Event, m EventMatch) bool {
	if m.Type != "" && e.Type != m.Type {
		return false
	}
	if m.Reason != "" && e.Reason != m.Reason {
		return false
	}
	return true
}

func summarizeEvents(events []eventsv1.Event) string {
	if len(events) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(events))
	for _, e := range events {
		parts = append(parts, fmt.Sprintf("%s/%s: %s", e.Type, e.Reason, e.Note))
	}
	return strings.Join(parts, "; ")
}
