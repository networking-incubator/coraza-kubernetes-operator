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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

func TestPublicationReadinessColdReplicaStaysUnready(t *testing.T) {
	readiness := NewPublicationReadiness()
	if err := readiness.Check(); err == nil {
		t.Fatal("Check() = nil before the initial sync")
	}

	accepted := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout"},
		Status: &wafv1alpha1.EngineStatus{
			AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{
				RenderingSchemaVersion: render.RenderingSchemaVersion,
				Version:                "sha256:v1",
			},
		},
	}
	withoutAccepted := &wafv1alpha1.Engine{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "other"}}
	readiness.Initialize([]*wafv1alpha1.Engine{accepted, withoutAccepted})

	err := readiness.Check()
	if err == nil || !strings.Contains(err.Error(), "shop/checkout") {
		t.Fatalf("Check() = %v, want a failure naming shop/checkout", err)
	}

	if !readiness.RecordFailure("shop", "checkout", errors.New("cannot reconstruct")) {
		t.Fatal("RecordFailure() = false for a pending Engine")
	}
	err = readiness.Check()
	if err == nil || !strings.Contains(err.Error(), "cannot reconstruct") {
		t.Fatalf("Check() = %v, want the reconstruction reason", err)
	}

	readiness.RecordSuccess("shop", "checkout")
	if err := readiness.Check(); err != nil {
		t.Fatalf("Check() = %v after a successful reconstruction", err)
	}
}

func TestPublicationReadinessKeepsReconcilesThatRunBeforeInitialize(t *testing.T) {
	accepted := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout"},
		Status: &wafv1alpha1.EngineStatus{
			AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{
				RenderingSchemaVersion: render.RenderingSchemaVersion,
				Version:                "sha256:v1",
			},
		},
	}
	failing := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "broken"},
		Status: &wafv1alpha1.EngineStatus{
			AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{
				RenderingSchemaVersion: render.RenderingSchemaVersion,
				Version:                "sha256:v1",
			},
		},
	}

	readiness := NewPublicationReadiness()
	readiness.RecordSuccess("shop", "checkout")
	readiness.RecordFailure("shop", "broken", errors.New("cold failure"))
	readiness.Initialize([]*wafv1alpha1.Engine{accepted, failing})

	err := readiness.Check()
	if err == nil || !strings.Contains(err.Error(), "shop/broken") {
		t.Fatalf("Check() = %v, want a failure naming shop/broken", err)
	}
	if strings.Contains(err.Error(), "shop/checkout") {
		t.Fatalf("Check() = %v, a pre-initialize success must not be pending", err)
	}
}

// TestPublicationReadinessIgnoresOtherRenderingSchemas checks that an accepted
// record this build cannot publish by design does not gate readiness, while a
// failure at the current schema still does.
func TestPublicationReadinessIgnoresOtherRenderingSchemas(t *testing.T) {
	oldSchema := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "upgraded"},
		Status: &wafv1alpha1.EngineStatus{
			AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{
				RenderingSchemaVersion: render.RenderingSchemaVersion - 1,
				Version:                "sha256:v1",
			},
		},
	}
	current := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "current"},
		Status: &wafv1alpha1.EngineStatus{
			AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{
				RenderingSchemaVersion: render.RenderingSchemaVersion,
				Version:                "sha256:v1",
			},
		},
	}

	readiness := NewPublicationReadiness()
	readiness.Initialize([]*wafv1alpha1.Engine{oldSchema})
	if err := readiness.Check(); err != nil {
		t.Fatalf("Check() = %v, an older rendering schema must not gate readiness", err)
	}

	readiness = NewPublicationReadiness()
	readiness.Initialize([]*wafv1alpha1.Engine{current})
	err := readiness.Check()
	if err == nil || !strings.Contains(err.Error(), "shop/current") {
		t.Fatalf("Check() = %v, a same-schema accepted record must still gate readiness", err)
	}
}

func TestPublicationReadinessLateFailuresDoNotFlipReadiness(t *testing.T) {
	readiness := NewPublicationReadiness()
	readiness.Initialize(nil)
	if err := readiness.Check(); err != nil {
		t.Fatalf("Check() = %v for an empty cluster", err)
	}

	if readiness.RecordFailure("shop", "checkout", errors.New("late failure")) {
		t.Fatal("RecordFailure() = true after the initial sync")
	}
	if err := readiness.Check(); err != nil {
		t.Fatalf("Check() = %v after a late failure", err)
	}
}

// TestPublicationReadinessExpiryWithoutMetrics covers the default tracker, which
// has no Metrics registered: the deadline must still release readiness instead
// of panicking on a nil counter.
func TestPublicationReadinessExpiryWithoutMetrics(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	readiness := NewPublicationReadiness()
	require.Nil(t, readiness.Metrics)
	readiness.now = func() time.Time { return now }
	readiness.deadline = time.Minute
	readiness.Initialize([]*wafv1alpha1.Engine{acceptedEngine("shop", "stuck")})

	now = now.Add(2 * time.Minute)
	if err := readiness.Check(); err != nil {
		t.Fatalf("Check() = %v after the deadline without metrics", err)
	}
}

// TestPublicationReadinessDeadlineBoundsPendingRecords checks that an accepted
// record this build cannot reconstruct stops gating readiness after the
// deadline, while it still gates before it.
func TestPublicationReadinessDeadlineBoundsPendingRecords(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}

	readiness := NewPublicationReadiness()
	readiness.Metrics = metrics
	readiness.now = func() time.Time { return now }
	readiness.deadline = time.Minute
	readiness.Initialize([]*wafv1alpha1.Engine{acceptedEngine("shop", "stuck")})

	now = now.Add(59 * time.Second)
	if err := readiness.Check(); err == nil {
		t.Fatal("Check() = nil before the deadline")
	}

	now = now.Add(2 * time.Second)
	if err := readiness.Check(); err != nil {
		t.Fatalf("Check() = %v after the deadline", err)
	}
	if got := testutil.ToFloat64(metrics.coldReconstruction); got != 1 {
		t.Fatalf("cold reconstruction counter = %v, want 1", got)
	}

	// The record is reported once; later probes stay ready.
	now = now.Add(time.Minute)
	if err := readiness.Check(); err != nil {
		t.Fatalf("Check() = %v after the deadline", err)
	}
	if got := testutil.ToFloat64(metrics.coldReconstruction); got != 1 {
		t.Fatalf("cold reconstruction counter = %v after a second check, want 1", got)
	}

	// A later failure of the same Engine must not count the cold record again.
	if readiness.RecordFailure("shop", "stuck", errors.New("still stuck")) {
		t.Fatal("RecordFailure() = true after the deadline expired the record")
	}
	if got := testutil.ToFloat64(metrics.coldReconstruction); got != 1 {
		t.Fatalf("cold reconstruction counter = %v after a post-deadline failure, want 1", got)
	}
}

// TestPublicationReadinessResolvedRecordIgnoresDeadline checks that a record
// reconstructed before the deadline makes readiness pass immediately.
func TestPublicationReadinessResolvedRecordIgnoresDeadline(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	readiness := NewPublicationReadiness()
	readiness.now = func() time.Time { return now }
	readiness.deadline = time.Minute
	readiness.Initialize([]*wafv1alpha1.Engine{acceptedEngine("shop", "checkout")})

	if !readiness.RecordFailure("shop", "checkout", errors.New("cannot reconstruct")) {
		t.Fatal("RecordFailure() = false for a pending Engine")
	}
	readiness.RecordSuccess("shop", "checkout")

	now = now.Add(2 * time.Minute)
	if err := readiness.Check(); err != nil {
		t.Fatalf("Check() = %v for a resolved record", err)
	}
}

func acceptedEngine(namespace, name string) *wafv1alpha1.Engine {
	return &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status: &wafv1alpha1.EngineStatus{
			AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{
				RenderingSchemaVersion: render.RenderingSchemaVersion,
				Version:                "sha256:v1",
			},
		},
	}
}

func TestVerifyBundledManifest(t *testing.T) {
	manifest, data, err := assets.ResolveDefault()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBundledManifest(manifest, data); err != nil {
		t.Fatalf("VerifyBundledManifest() error = %v", err)
	}
	if err := VerifyBundledManifest(manifest, []byte("corrupted")); err == nil {
		t.Fatal("VerifyBundledManifest() = nil for corrupted bytes")
	}
}
