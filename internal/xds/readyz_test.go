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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
)

func TestPublicationReadinessColdReplicaStaysUnready(t *testing.T) {
	readiness := NewPublicationReadiness()
	if err := readiness.Check(); err == nil {
		t.Fatal("Check() = nil before the initial sync")
	}

	accepted := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout"},
		Status: &wafv1alpha1.EngineStatus{
			AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{Version: "sha256:v1"},
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
			AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{Version: "sha256:v1"},
		},
	}
	failing := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "broken"},
		Status: &wafv1alpha1.EngineStatus{
			AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{Version: "sha256:v1"},
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
