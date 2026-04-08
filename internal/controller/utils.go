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
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// -----------------------------------------------------------------------------
// Logging Helpers
// -----------------------------------------------------------------------------

// debugLevel is the go-logr level for debug/verbose logging
const debugLevel = 1

// logInfo logs an info-level message with consistent structured context.
func logInfo(log logr.Logger, req ctrl.Request, kind, msg string, keysAndValues ...any) {
	args := append([]any{"namespace", req.Namespace, "name", req.Name}, keysAndValues...)
	log.Info(fmt.Sprintf("%s: %s", kind, msg), args...)
}

// logDebug logs a debug-level message with consistent structured context.
func logDebug(log logr.Logger, req ctrl.Request, kind, msg string, keysAndValues ...any) {
	args := append([]any{"namespace", req.Namespace, "name", req.Name}, keysAndValues...)
	log.V(debugLevel).Info(fmt.Sprintf("%s: %s", kind, msg), args...)
}

// logError logs an error-level message with consistent structured context.
func logError(log logr.Logger, req ctrl.Request, kind string, err error, msg string, keysAndValues ...any) {
	args := append([]any{"namespace", req.Namespace, "name", req.Name}, keysAndValues...)
	log.Error(err, fmt.Sprintf("%s: %s", kind, msg), args...)
}

// logAPIError logs an error from a Kubernetes API call, enriching it with
// resourceVersion (from obj, when non-nil), HTTP status code, API reason,
// and retryAfterSeconds when the error is or wraps a *apierrors.StatusError.
//
// Workqueue retry/attempt count is not available inside Reconcile without a
// cross-cutting wrapper or metrics integration; that is left for a follow-up.
func logAPIError(log logr.Logger, req ctrl.Request, kind string, err error, msg string, obj client.Object, extra ...any) {
	if len(extra)%2 != 0 {
		logDebug(log, req, kind, "logAPIError called with odd number of extra key-value arguments; dropping trailing orphan")
		extra = extra[:len(extra)-1]
	}

	args := append([]any{"namespace", req.Namespace, "name", req.Name}, extra...)

	if obj != nil {
		if rv := obj.GetResourceVersion(); rv != "" {
			args = append(args, "resourceVersion", rv)
		}
	}

	args = append(args, extractStatusErrorFields(err)...)
	log.Error(err, fmt.Sprintf("%s: %s", kind, msg), args...)
}

// extractStatusErrorFields returns structured key/value pairs from a
// *apierrors.StatusError if err is or wraps one.
func extractStatusErrorFields(err error) []any {
	var statusErr *apierrors.StatusError
	if !errors.As(err, &statusErr) {
		return nil
	}

	st := statusErr.Status()
	fields := []any{
		"apiStatusCode", st.Code,
		"apiReason", string(st.Reason),
	}

	if st.Details != nil && st.Details.RetryAfterSeconds > 0 {
		fields = append(fields, "retryAfterSeconds", st.Details.RetryAfterSeconds)
	}

	return fields
}

// -----------------------------------------------------------------------------
// Status Condition Helpers
// -----------------------------------------------------------------------------

// trackedConditionTypes are the operator-owned condition types whose transitions
// are logged at Info level.
var trackedConditionTypes = []string{"Ready", "Degraded", "Progressing"}

// conditionSnapshot captures the Status and Reason of each tracked condition
// type before mutation. A nil entry means the condition was absent.
type conditionSnapshot map[string]*metav1.Condition

func snapshotConditions(conditions []metav1.Condition) conditionSnapshot {
	snap := make(conditionSnapshot, len(trackedConditionTypes))
	for _, ct := range trackedConditionTypes {
		if c := apimeta.FindStatusCondition(conditions, ct); c != nil {
			cpy := *c
			snap[ct] = &cpy
		}
	}
	return snap
}

// logConditionTransitions compares before and after snapshots and logs at Info
// for each tracked condition whose Status changed, appeared, or disappeared.
// Idempotent no-ops (same Status+Reason) are silent.
func logConditionTransitions(log logr.Logger, req ctrl.Request, kind string, before conditionSnapshot, after []metav1.Condition) {
	for _, ct := range trackedConditionTypes {
		prev := before[ct]
		cur := apimeta.FindStatusCondition(after, ct)

		switch {
		case prev == nil && cur == nil:
			continue
		case prev == nil && cur != nil:
			logInfo(log, req, kind, "Condition set",
				"condition", ct,
				"fromStatus", "",
				"toStatus", string(cur.Status),
				"reason", cur.Reason)
		case prev != nil && cur == nil:
			logInfo(log, req, kind, "Condition removed",
				"condition", ct,
				"fromStatus", string(prev.Status),
				"toStatus", "")
		default:
			if prev.Status == cur.Status && prev.Reason == cur.Reason {
				continue
			}
			logInfo(log, req, kind, "Condition changed",
				"condition", ct,
				"fromStatus", string(prev.Status),
				"toStatus", string(cur.Status),
				"reason", cur.Reason)
		}
	}
}

// setConditionTrue is a helper function to set metav1.Conditions to True.
func setConditionTrue(conditions *[]metav1.Condition, generation int64, conditionType, reason, message string) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})
}

// setConditionFalse is a helper function to set metav1.Conditions to False.
func setConditionFalse(conditions *[]metav1.Condition, generation int64, conditionType, reason, message string) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})
}

// applyStatusConditionDegraded mutates conditions to a Degraded state. It does
// not log; call logConditionTransitions after a successful Status().Patch.
func applyStatusConditionDegraded(conditions *[]metav1.Condition, generation int64, reason, message string) {
	setConditionFalse(conditions, generation, "Ready", reason, message)
	setConditionTrue(conditions, generation, "Degraded", reason, message)
	apimeta.RemoveStatusCondition(conditions, "Progressing")
}

// applyStatusProgressing mutates conditions to a Progressing state. It does not
// log; call logConditionTransitions after a successful Status().Patch.
func applyStatusProgressing(conditions *[]metav1.Condition, generation int64, reason, message string) {
	setConditionFalse(conditions, generation, "Ready", reason, message)
	setConditionTrue(conditions, generation, "Progressing", reason, message)
}

// maxEventNoteBytes is the maximum size of the note field in events.k8s.io/v1.
// Events exceeding this limit are silently rejected by the API server.
const maxEventNoteBytes = 1024

// truncateEventNote truncates a message to fit within the events v1 API note
// field limit. Status condition messages are unaffected by this limit.
func truncateEventNote(msg string) string {
	if len(msg) <= maxEventNoteBytes {
		return msg
	}
	return msg[:maxEventNoteBytes-3] + "..."
}

// patchDegraded marks a resource as Degraded, emits a Warning event, and
// patches the status in a single call. It consolidates the repeated pattern of
// Eventf → DeepCopy → apply status → Status().Patch → logConditionTransitions on success.
func patchDegraded(
	ctx context.Context,
	statusWriter client.StatusWriter,
	recorder events.EventRecorder,
	log logr.Logger,
	req ctrl.Request,
	kind string,
	obj client.Object,
	conditions *[]metav1.Condition,
	generation int64,
	reason, message string,
) error {
	recorder.Eventf(obj, nil, "Warning", reason, "Reconcile", truncateEventNote(message))
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	before := snapshotConditions(*conditions)
	applyStatusConditionDegraded(conditions, generation, reason, message)
	if err := statusWriter.Patch(ctx, obj, patch); err != nil {
		logAPIError(log, req, kind, err, "Failed to patch status", obj)
		return err
	}
	logConditionTransitions(log, req, kind, before, *conditions)
	return nil
}

// applyStatusReady mutates conditions to Ready. It does not log; call
// logConditionTransitions after a successful Status().Patch.
func applyStatusReady(conditions *[]metav1.Condition, generation int64, reason, message string) {
	setConditionTrue(conditions, generation, "Ready", reason, message)
	apimeta.RemoveStatusCondition(conditions, "Degraded")
	apimeta.RemoveStatusCondition(conditions, "Progressing")
}

// patchReady marks a resource as Ready, emits a Normal event, and patches the
// status in a single call. It mirrors patchDegraded for the success path.
func patchReady(
	ctx context.Context,
	statusWriter client.StatusWriter,
	recorder events.EventRecorder,
	log logr.Logger,
	req ctrl.Request,
	kind string,
	obj client.Object,
	conditions *[]metav1.Condition,
	generation int64,
	reason, message string,
) error {
	recorder.Eventf(obj, nil, "Normal", reason, "Reconcile", truncateEventNote(message))
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	before := snapshotConditions(*conditions)
	applyStatusReady(conditions, generation, reason, message)
	if err := statusWriter.Patch(ctx, obj, patch); err != nil {
		logAPIError(log, req, kind, err, "Failed to patch status", obj)
		return err
	}
	logConditionTransitions(log, req, kind, before, *conditions)
	return nil
}

// -----------------------------------------------------------------------------
// Watch Mapper Helpers
// -----------------------------------------------------------------------------

// collectRequests filters a slice of Kubernetes objects by a predicate and
// returns reconcile.Requests for each match.
func collectRequests[E any, P interface {
	*E
	client.Object
}](items []E, match func(P) bool) []reconcile.Request {
	var requests []reconcile.Request
	for i := range items {
		p := P(&items[i])
		if match(p) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      p.GetName(),
					Namespace: p.GetNamespace(),
				},
			})
		}
	}
	return requests
}

// -----------------------------------------------------------------------------
// Predicate Helpers
// -----------------------------------------------------------------------------

// annotationChangedPredicate returns a predicate that triggers on Update events
// when the value of the specified annotation key differs between old and new objects.
func annotationChangedPredicate(key string) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return false },
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			return e.ObjectOld.GetAnnotations()[key] != e.ObjectNew.GetAnnotations()[key]
		},
	}
}

// -----------------------------------------------------------------------------
// Client Operation Helpers
// -----------------------------------------------------------------------------

// fieldManager is the server-side apply field manager name for this operator.
const fieldManager = "coraza-kubernetes-operator"

// serverSideApply applies an unstructured Kubernetes object using server-side
// apply. This avoids the optimistic concurrency conflicts inherent in
// Get-then-Update patterns by using field ownership for conflict detection.
//
// The desired object must have its GVK and name set.
func serverSideApply(ctx context.Context, c client.Client, desired *unstructured.Unstructured) error {
	gvk := desired.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		return errors.New("desired object must have GroupVersionKind set")
	}

	if desired.GetName() == "" {
		return errors.New("desired object must have a name set")
	}
	if desired.GetNamespace() == "" {
		desired.SetNamespace(corev1.NamespaceDefault)
	}

	if err := c.Patch(ctx, desired, client.Apply, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("server-side apply %s %s/%s: %w", gvk.Kind, desired.GetNamespace(), desired.GetName(), err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Error Messaging Helpers
// -----------------------------------------------------------------------------

func sanitizeErrorMessage(err error) error {
	matches := sanitizeFilePath.FindStringSubmatch(err.Error())

	if len(matches) < 2 {
		return err
	}

	// matches[1] is the full path. filepath.Base pulls the last element.
	fileName := filepath.Base(matches[1])

	return fmt.Errorf("open %s: data does not exist", fileName)

}

// shouldSkipMissingFileError reports whether a missing-file validation error should
// be skipped because the file is present in secretData.
func shouldSkipMissingFileError(err error, secretData map[string][]byte) bool {
	if secretData == nil {
		return false
	}

	matches := sanitizeFilePath.FindStringSubmatch(err.Error())
	if len(matches) < 2 {
		return false
	}

	fileName := filepath.Base(matches[1])

	_, exists := secretData[fileName]
	return exists
}

// buildCacheReadyMessage constructs the Ready condition message after successful
// caching. When unsupportedMsg is non-empty (annotation override active), the
// detected unsupported rules are appended so they remain visible in the status.
func buildCacheReadyMessage(namespace, name, unsupportedMsg string) string {
	msg := fmt.Sprintf("Successfully cached rules for %s/%s", namespace, name)
	if unsupportedMsg != "" {
		msg += "\n[annotation override] " + unsupportedMsg
	}

	return msg
}
