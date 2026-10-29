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

import rbacv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"

// Bootstrap policies. A fail-closed bootstrap denies traffic until the first
// accepted configuration arrives; fail-open allows it.
const (
	bootstrapPolicyFail  = "fail"
	bootstrapPolicyAllow = "allow"

	rbacActionDeny = "DENY"
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
	if desired == bootstrapPolicyAllow {
		// Weakening requires either an already-weakened filter or an existing
		// filter plus an active accepted configuration. With no previous filter
		// the first attachment must be fail-closed, even if the Engine status
		// already records the accepted configuration.
		if previous == bootstrapPolicyAllow || (previous != "" && acceptedActive) {
			return bootstrapPolicyAllow
		}
	}
	return bootstrapPolicyFail
}

// bootstrapRBACActionEnum returns the typed action. ALLOW with no matching
// policy denies; DENY with no matching policy allows.
func bootstrapRBACActionEnum(policy string) rbacv3.RBAC_Action {
	if policy == bootstrapPolicyAllow {
		return rbacv3.RBAC_DENY
	}
	return rbacv3.RBAC_ALLOW
}
