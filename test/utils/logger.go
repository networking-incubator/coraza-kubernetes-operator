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

// Package utils provides testing utilities for integration and unit tests.
package utils

import (
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
)

// -----------------------------------------------------------------------------
// Test Event Recorder (no-op)
// -----------------------------------------------------------------------------

var (
	_ events.EventRecorderLogger = (*testRecorder)(nil)
	_ events.EventRecorderLogger = (*FakeRecorder)(nil)
)

type testRecorder struct{}

// NewTestRecorder creates a no-op event recorder for testing.
func NewTestRecorder() events.EventRecorder {
	return &testRecorder{}
}

// Eventf implements events.EventRecorder.
func (r *testRecorder) Eventf(runtime.Object, runtime.Object, string, string, string, string, ...any) {
}

// WithLogger implements events.EventRecorderLogger.
func (r *testRecorder) WithLogger(logger klog.Logger) events.EventRecorderLogger {
	return r
}

// -----------------------------------------------------------------------------
// Fake Event Recorder (captures events for assertions)
// -----------------------------------------------------------------------------

// RecordedEvent holds a single event captured by FakeRecorder.
type RecordedEvent struct {
	Type   string
	Reason string
	Action string
	Note   string
}

// FakeRecorder captures events for later inspection in tests.
type FakeRecorder struct {
	Events []RecordedEvent
}

// NewFakeRecorder creates a recorder that captures events instead of
// discarding them.
func NewFakeRecorder() *FakeRecorder {
	return &FakeRecorder{}
}

// Eventf implements events.EventRecorder.
func (r *FakeRecorder) Eventf(_ runtime.Object, _ runtime.Object, eventtype, reason, action, note string, args ...any) {
	r.Events = append(r.Events, RecordedEvent{
		Type:   eventtype,
		Reason: reason,
		Action: action,
		Note:   fmt.Sprintf(note, args...),
	})
}

// WithLogger implements events.EventRecorderLogger.
func (r *FakeRecorder) WithLogger(_ klog.Logger) events.EventRecorderLogger {
	return r
}

// HasEvent returns true if any recorded event matches the given type and reason.
func (r *FakeRecorder) HasEvent(eventType, reason string) bool {
	for _, e := range r.Events {
		if e.Type == eventType && e.Reason == reason {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Test Logger
// -----------------------------------------------------------------------------

type testLogger struct {
	t *testing.T
}

// NewTestLogger creates a logr.Logger that logs via testing.T
func NewTestLogger(t *testing.T) logr.Logger {
	return logr.New(&testLogger{t: t})
}

// -----------------------------------------------------------------------------
// Test Logger - LogSink Implementation
// -----------------------------------------------------------------------------

// Init initializes the logger with runtime information
func (l *testLogger) Init(info logr.RuntimeInfo) {}

// Enabled returns whether logging is enabled at the given level
func (l *testLogger) Enabled(level int) bool {
	return true // always true for testing
}

// Info logs informational messages to the test output
func (l *testLogger) Info(level int, msg string, keysAndValues ...any) {
	l.t.Logf("[INFO] %s %v", msg, keysAndValues)
}

// Error logs error messages to the test output
func (l *testLogger) Error(err error, msg string, keysAndValues ...any) {
	l.t.Logf("[ERROR] %s: %v %v", msg, err, keysAndValues)
}

// WithValues returns the logger with additional key-value pairs
func (l *testLogger) WithValues(keysAndValues ...any) logr.LogSink {
	return l
}

// WithName returns the logger with an additional name segment
func (l *testLogger) WithName(name string) logr.LogSink {
	return l
}
