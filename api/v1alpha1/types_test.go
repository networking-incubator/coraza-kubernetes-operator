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

package v1alpha1

import (
	"strings"
	"testing"
)

func TestEngineStatusDeepCopy(t *testing.T) {
	engine := &Engine{
		Status: &EngineStatus{
			AcceptedConfiguration: &EngineAcceptedConfiguration{
				ObservedGeneration:     3,
				RenderingSchemaVersion: 1,
				Version:                "sha256:" + strings.Repeat("a", 64),
				RuleSetUID:             "uid",
				RuleSetContentHash:     "sha256:" + strings.Repeat("b", 64),
				BinaryURL:              "https://example.com/plugin.wasm",
				WasmSHA256:             strings.Repeat("c", 64),
				FailurePolicy:          FailurePolicyFail,
			},
		},
	}

	copied := engine.DeepCopy()
	copied.Status.AcceptedConfiguration.Version = "changed"

	if engine.Status.AcceptedConfiguration.Version == "changed" {
		t.Fatal("DeepCopy shared acceptedConfiguration with the original")
	}
}

func TestRuleSetStatusDeepCopy(t *testing.T) {
	ruleSet := &RuleSet{
		Status: RuleSetStatus{
			AcceptedContent: &RuleSetAcceptedContent{
				ObservedGeneration: 2,
				ContentHash:        "sha256:" + strings.Repeat("d", 64),
			},
		},
	}

	copied := ruleSet.DeepCopy()
	copied.Status.AcceptedContent.ContentHash = "changed"

	if ruleSet.Status.AcceptedContent.ContentHash == "changed" {
		t.Fatal("DeepCopy shared acceptedContent with the original")
	}
}

func TestEmptyRulesPolicyValues(t *testing.T) {
	if EmptyRulesPolicyReject != "Reject" || EmptyRulesPolicyAllow != "Allow" {
		t.Fatalf("unexpected policy values: %q, %q", EmptyRulesPolicyReject, EmptyRulesPolicyAllow)
	}
}
