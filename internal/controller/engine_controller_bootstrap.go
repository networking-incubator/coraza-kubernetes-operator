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

// Bootstrap policies. A fail-closed bootstrap denies traffic until the first
// accepted configuration arrives; fail-open allows it.
const (
	bootstrapPolicyFail  = "fail"
	bootstrapPolicyAllow = "allow"

	rbacActionAllow = "ALLOW"
	rbacActionDeny  = "DENY"
)

// bootstrapPolicy returns the policy to apply now, given the previous applied
// policy, the desired policy, and whether a matching accepted configuration is
// already active. It implements the stricter-of rule.
//
// Tightening applies immediately. Weakening from fail to allow is deferred
// until the matching configuration is accepted and active, so a Gateway can
// never run with the weaker default before its WAF is live. Unknown values
// fail closed.
func bootstrapPolicy(previous, desired string, acceptedActive bool) string {
	if desired == bootstrapPolicyAllow && (previous == bootstrapPolicyAllow || acceptedActive) {
		return bootstrapPolicyAllow
	}
	return bootstrapPolicyFail
}

// bootstrapRBACAction maps an applied policy to the RBAC default config action.
// ALLOW with no matching policy denies; DENY with no matching policy allows.
func bootstrapRBACAction(policy string) string {
	if policy == bootstrapPolicyAllow {
		return rbacActionDeny
	}
	return rbacActionAllow
}
