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

package controller

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// ---------------------------------------------------------------------------
// captureSink — records log entries for assertions
// ---------------------------------------------------------------------------

type captureSink struct {
	entries []logEntry
}

type logEntry struct {
	Level         int
	Msg           string
	KeysAndValues []any
	Err           error
}

func (s *captureSink) Init(logr.RuntimeInfo)          {}
func (s *captureSink) Enabled(int) bool               { return true }
func (s *captureSink) WithValues(...any) logr.LogSink { return s }
func (s *captureSink) WithName(string) logr.LogSink   { return s }

func (s *captureSink) Info(level int, msg string, keysAndValues ...any) {
	s.entries = append(s.entries, logEntry{Level: level, Msg: msg, KeysAndValues: keysAndValues})
}

func (s *captureSink) Error(err error, msg string, keysAndValues ...any) {
	s.entries = append(s.entries, logEntry{Level: -1, Msg: msg, KeysAndValues: keysAndValues, Err: err})
}

func newCaptureLogger() (logr.Logger, *captureSink) {
	sink := &captureSink{}
	return logr.New(sink), sink
}

// infoEntries returns log lines emitted at default Info severity (go-logr level 0).
func (s *captureSink) infoEntries() []logEntry {
	var out []logEntry
	for _, e := range s.entries {
		if e.Level == 0 {
			out = append(out, e)
		}
	}
	return out
}

// findInfoEntry returns the first default-Info log (level 0) whose message contains substr.
func (s *captureSink) findInfoEntry(substr string) *logEntry {
	for i := range s.entries {
		if s.entries[i].Level == 0 && strings.Contains(s.entries[i].Msg, substr) {
			return &s.entries[i]
		}
	}
	return nil
}

// findInfoEntryByCondition returns the first default-Info log where keys include
// "condition" == conditionType (avoids brittle substring matching when multiple
// transitions occur in one reconcile).
func (s *captureSink) findInfoEntryByCondition(conditionType string) *logEntry {
	for i := range s.entries {
		if s.entries[i].Level != 0 {
			continue
		}
		kv := kvMap(s.entries[i].KeysAndValues)
		if v, ok := kv["condition"].(string); ok && v == conditionType {
			return &s.entries[i]
		}
	}
	return nil
}

func kvMap(kvs []any) map[string]any {
	m := make(map[string]any, len(kvs)/2)
	for i := 0; i+1 < len(kvs); i += 2 {
		if k, ok := kvs[i].(string); ok {
			m[k] = kvs[i+1]
		}
	}
	return m
}

func testReq() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "obj"}}
}

// ---------------------------------------------------------------------------
// Condition Transition Tests
// ---------------------------------------------------------------------------

func TestLogConditionTransitions(t *testing.T) {
	req := testReq()

	t.Run("no prior conditions -> applyStatusProgressing logs new conditions", func(t *testing.T) {
		log, sink := newCaptureLogger()
		var conditions []metav1.Condition

		before := snapshotConditions(conditions)
		applyStatusProgressing(&conditions, 1, "Reconciling", "starting")
		logConditionTransitions(log, req, "Engine", before, conditions)

		infos := sink.infoEntries()
		require.NotEmpty(t, infos, "expected Info log for new conditions")

		readySet := sink.findInfoEntry("Condition set")
		require.NotNil(t, readySet, "expected 'Condition set' for Ready=False")
		assert.Zero(t, readySet.Level, "condition transitions must use log.Info (level 0), not log.V")
	})

	t.Run("idempotent call does not log transitions", func(t *testing.T) {
		log, sink := newCaptureLogger()
		conditions := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "RulesCached"},
		}

		before := snapshotConditions(conditions)
		applyStatusReady(&conditions, 1, "RulesCached", "cached")
		logConditionTransitions(log, req, "RuleSet", before, conditions)

		for _, e := range sink.entries {
			assert.NotContains(t, e.Msg, "Condition changed")
			assert.NotContains(t, e.Msg, "Condition set")
		}
	})

	t.Run("message-only update same status and reason stays silent", func(t *testing.T) {
		log, sink := newCaptureLogger()
		conditions := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Configured", Message: "old detail"},
		}

		before := snapshotConditions(conditions)
		applyStatusReady(&conditions, 1, "Configured", "new detail")
		logConditionTransitions(log, req, "Engine", before, conditions)

		assert.Empty(t, sink.infoEntries(), "Status+Reason unchanged: no Info noise")
	})

	t.Run("Progressing -> Ready logs Ready change and Progressing removal", func(t *testing.T) {
		log, sink := newCaptureLogger()
		conditions := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Reconciling"},
			{Type: "Progressing", Status: metav1.ConditionTrue, Reason: "Reconciling"},
		}

		before := snapshotConditions(conditions)
		applyStatusReady(&conditions, 1, "Configured", "done")
		logConditionTransitions(log, req, "Engine", before, conditions)

		changedEntry := sink.findInfoEntryByCondition("Ready")
		require.NotNil(t, changedEntry)
		assert.Contains(t, changedEntry.Msg, "Condition changed")
		assert.Zero(t, changedEntry.Level)
		kv := kvMap(changedEntry.KeysAndValues)
		assert.Equal(t, "Ready", kv["condition"])
		assert.Equal(t, "False", kv["fromStatus"])
		assert.Equal(t, "True", kv["toStatus"])

		removedEntry := sink.findInfoEntryByCondition("Progressing")
		require.NotNil(t, removedEntry)
		assert.Contains(t, removedEntry.Msg, "Condition removed")
		assert.Zero(t, removedEntry.Level)
		rkv := kvMap(removedEntry.KeysAndValues)
		assert.Equal(t, "Progressing", rkv["condition"])
	})

	t.Run("Ready -> Degraded logs transitions", func(t *testing.T) {
		log, sink := newCaptureLogger()
		conditions := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Configured"},
		}

		before := snapshotConditions(conditions)
		applyStatusConditionDegraded(&conditions, 1, "Error", "something broke")
		logConditionTransitions(log, req, "Engine", before, conditions)

		infos := sink.infoEntries()
		require.GreaterOrEqual(t, len(infos), 2, "expect Ready change + Degraded set")

		readyChanged := sink.findInfoEntry("Condition changed")
		require.NotNil(t, readyChanged)
		assert.Zero(t, readyChanged.Level)
		rkv := kvMap(readyChanged.KeysAndValues)
		assert.Equal(t, "Ready", rkv["condition"])
		assert.Equal(t, "True", rkv["fromStatus"])
		assert.Equal(t, "False", rkv["toStatus"])
	})

	t.Run("same status different reason still logs", func(t *testing.T) {
		log, sink := newCaptureLogger()
		conditions := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse, Reason: "OldReason"},
			{Type: "Degraded", Status: metav1.ConditionTrue, Reason: "OldReason"},
		}

		before := snapshotConditions(conditions)
		applyStatusConditionDegraded(&conditions, 1, "NewReason", "updated")
		logConditionTransitions(log, req, "RuleSet", before, conditions)

		entry := sink.findInfoEntry("Condition changed")
		require.NotNil(t, entry, "reason-only change should log")
		assert.Zero(t, entry.Level)
		kv := kvMap(entry.KeysAndValues)
		assert.Equal(t, "NewReason", kv["reason"])
	})

	t.Run("applyStatusProgressing adds Progressing when absent", func(t *testing.T) {
		log, sink := newCaptureLogger()
		conditions := []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Error"},
			{Type: "Degraded", Status: metav1.ConditionTrue, Reason: "Error"},
		}

		before := snapshotConditions(conditions)
		applyStatusProgressing(&conditions, 2, "Retrying", "retry")
		logConditionTransitions(log, req, "RuleSet", before, conditions)

		progressingSet := sink.findInfoEntry("Condition set")
		require.NotNil(t, progressingSet)
		assert.Zero(t, progressingSet.Level)
		kv := kvMap(progressingSet.KeysAndValues)
		assert.Equal(t, "Progressing", kv["condition"])

		readyChanged := sink.findInfoEntry("Condition changed")
		require.NotNil(t, readyChanged)
		assert.Zero(t, readyChanged.Level)
		rkv := kvMap(readyChanged.KeysAndValues)
		assert.Equal(t, "Ready", rkv["condition"])
		assert.Equal(t, "Retrying", rkv["reason"])
	})
}

// ---------------------------------------------------------------------------
// extractStatusErrorFields Tests
// ---------------------------------------------------------------------------

func TestExtractStatusErrorFields(t *testing.T) {
	t.Run("non-StatusError returns nil", func(t *testing.T) {
		fields := extractStatusErrorFields(fmt.Errorf("plain error"))
		assert.Nil(t, fields)
	})

	t.Run("StatusError returns code and reason", func(t *testing.T) {
		err := apierrors.NewNotFound(
			schema.GroupResource{Group: "waf.k8s.coraza.io", Resource: "engines"}, "my-engine")

		fields := extractStatusErrorFields(err)
		require.NotNil(t, fields)
		kv := kvMap(fields)
		assert.Equal(t, int32(http.StatusNotFound), kv["apiStatusCode"])
		assert.Equal(t, string(metav1.StatusReasonNotFound), kv["apiReason"])
	})

	t.Run("wrapped StatusError is detected", func(t *testing.T) {
		inner := apierrors.NewConflict(
			schema.GroupResource{Resource: "engines"}, "my-engine", fmt.Errorf("conflict"))
		wrapped := fmt.Errorf("fetching engine: %w", inner)

		fields := extractStatusErrorFields(wrapped)
		require.NotNil(t, fields)
		kv := kvMap(fields)
		assert.Equal(t, int32(http.StatusConflict), kv["apiStatusCode"])
	})

	t.Run("StatusError with RetryAfterSeconds includes field", func(t *testing.T) {
		err := &apierrors.StatusError{
			ErrStatus: metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    http.StatusTooManyRequests,
				Reason:  metav1.StatusReasonTooManyRequests,
				Details: &metav1.StatusDetails{RetryAfterSeconds: 30},
			},
		}

		fields := extractStatusErrorFields(err)
		kv := kvMap(fields)
		assert.Equal(t, int32(30), kv["retryAfterSeconds"])
		assert.Equal(t, int32(http.StatusTooManyRequests), kv["apiStatusCode"])
	})

	t.Run("StatusError without RetryAfterSeconds omits field", func(t *testing.T) {
		err := apierrors.NewInternalError(fmt.Errorf("internal"))

		fields := extractStatusErrorFields(err)
		kv := kvMap(fields)
		_, hasRetry := kv["retryAfterSeconds"]
		assert.False(t, hasRetry)
	})
}

// ---------------------------------------------------------------------------
// logAPIError Tests
// ---------------------------------------------------------------------------

func TestLogAPIError(t *testing.T) {
	req := testReq()

	t.Run("nil obj omits resourceVersion", func(t *testing.T) {
		log, sink := newCaptureLogger()
		logAPIError(log, req, "Engine", fmt.Errorf("test"), "Failed to get", nil)

		require.Len(t, sink.entries, 1)
		kv := kvMap(sink.entries[0].KeysAndValues)
		_, hasRV := kv["resourceVersion"]
		assert.False(t, hasRV)
	})

	t.Run("obj with resourceVersion adds it", func(t *testing.T) {
		log, sink := newCaptureLogger()

		obj := &unstructured.Unstructured{}
		obj.SetResourceVersion("12345")

		err := apierrors.NewNotFound(schema.GroupResource{Resource: "engines"}, "eng")
		logAPIError(log, req, "Engine", err, "Failed to get", obj)

		require.Len(t, sink.entries, 1)
		kv := kvMap(sink.entries[0].KeysAndValues)
		assert.Equal(t, "12345", kv["resourceVersion"])
		assert.Equal(t, int32(http.StatusNotFound), kv["apiStatusCode"])
	})

	t.Run("non-nil obj with empty resourceVersion omits key", func(t *testing.T) {
		log, sink := newCaptureLogger()
		obj := &unstructured.Unstructured{}
		obj.SetName("x")

		err := apierrors.NewTimeoutError("slow", 0)
		logAPIError(log, req, "Engine", err, "Failed", obj)

		kv := kvMap(sink.entries[0].KeysAndValues)
		_, hasRV := kv["resourceVersion"]
		assert.False(t, hasRV)
	})

	t.Run("extra key-value pairs are included", func(t *testing.T) {
		log, sink := newCaptureLogger()
		logAPIError(log, req, "RuleSet", fmt.Errorf("err"), "oops", nil, "secretName", "my-secret")

		require.Len(t, sink.entries, 1)
		kv := kvMap(sink.entries[0].KeysAndValues)
		assert.Equal(t, "my-secret", kv["secretName"])
	})

	t.Run("odd extra args with single orphan drops it", func(t *testing.T) {
		log, sink := newCaptureLogger()
		logAPIError(log, req, "Engine", fmt.Errorf("err"), "Failed", nil, "orphanKey")

		var hasDebugWarning bool
		for _, e := range sink.entries {
			if e.Level == debugLevel && strings.Contains(e.Msg, "odd number of extra") {
				hasDebugWarning = true
			}
		}
		assert.True(t, hasDebugWarning, "expected debug warning about odd extra args")

		errorEntries := 0
		for _, e := range sink.entries {
			if e.Level == -1 {
				errorEntries++
				kv := kvMap(e.KeysAndValues)
				_, hasOrphan := kv["orphanKey"]
				assert.False(t, hasOrphan, "orphan key should not appear in log fields")
				assert.Equal(t, "ns", kv["namespace"], "namespace must still be logged on error path")
				assert.Equal(t, "obj", kv["name"], "name must still be logged on error path")
			}
		}
		assert.Equal(t, 1, errorEntries, "error should still be logged")
	})

	t.Run("odd extra args preserves valid pairs before trailing orphan", func(t *testing.T) {
		log, sink := newCaptureLogger()
		logAPIError(log, req, "Engine", fmt.Errorf("err"), "Failed", nil,
			"secretName", "my-secret", "orphanKey")

		var errorEntry *logEntry
		for i := range sink.entries {
			if sink.entries[i].Level == -1 {
				errorEntry = &sink.entries[i]
			}
		}
		require.NotNil(t, errorEntry, "error should still be logged")
		kv := kvMap(errorEntry.KeysAndValues)
		assert.Equal(t, "my-secret", kv["secretName"], "valid pair before orphan must be preserved")
		_, hasOrphan := kv["orphanKey"]
		assert.False(t, hasOrphan, "trailing orphan must be dropped")
	})

	t.Run("odd extra args preserves multiple pairs before trailing orphan", func(t *testing.T) {
		log, sink := newCaptureLogger()
		logAPIError(log, req, "Engine", fmt.Errorf("err"), "Failed", nil,
			"k1", "v1", "k2", "v2", "orphan")

		var errorEntry *logEntry
		for i := range sink.entries {
			if sink.entries[i].Level == -1 {
				errorEntry = &sink.entries[i]
			}
		}
		require.NotNil(t, errorEntry)
		kv := kvMap(errorEntry.KeysAndValues)
		assert.Equal(t, "v1", kv["k1"])
		assert.Equal(t, "v2", kv["k2"])
		_, hasOrphan := kv["orphan"]
		assert.False(t, hasOrphan)
	})
}
