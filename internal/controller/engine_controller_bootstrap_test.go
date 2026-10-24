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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBootstrapPolicy(t *testing.T) {
	tests := []struct {
		name           string
		previous       string
		desired        string
		acceptedActive bool
		want           string
	}{
		{name: "fail stays fail", previous: "fail", desired: "fail", acceptedActive: false, want: "fail"},
		{name: "fail stays fail when accepted", previous: "fail", desired: "fail", acceptedActive: true, want: "fail"},
		{name: "tighten applies immediately", previous: "allow", desired: "fail", acceptedActive: false, want: "fail"},
		{name: "tighten applies immediately when accepted", previous: "allow", desired: "fail", acceptedActive: true, want: "fail"},
		{name: "weaken is deferred without acceptance", previous: "fail", desired: "allow", acceptedActive: false, want: "fail"},
		{name: "weaken applies after acceptance", previous: "fail", desired: "allow", acceptedActive: true, want: "allow"},
		{name: "allow stays allow", previous: "allow", desired: "allow", acceptedActive: false, want: "allow"},
		{name: "allow stays allow when accepted", previous: "allow", desired: "allow", acceptedActive: true, want: "allow"},
		{name: "unknown previous fails closed", previous: "", desired: "allow", acceptedActive: false, want: "fail"},
		{name: "unknown previous weakens after acceptance", previous: "", desired: "allow", acceptedActive: true, want: "allow"},
		{name: "unknown desired fails closed", previous: "allow", desired: "", acceptedActive: true, want: "fail"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bootstrapPolicy(tc.previous, tc.desired, tc.acceptedActive))
		})
	}
}

func TestBootstrapRBACAction(t *testing.T) {
	assert.Equal(t, "ALLOW", bootstrapRBACAction("fail"))
	assert.Equal(t, "DENY", bootstrapRBACAction("allow"))
	assert.Equal(t, "ALLOW", bootstrapRBACAction(""), "unknown policies must fail closed")
}
