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
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

// resourceNamePrefix is the shared CKO/plugin resource naming prefix; it must
// match the prefix render.ResourceName builds.
const resourceNamePrefix = "coraza.waf."

// Snapshot is one immutable published Engine configuration. Callers must not
// mutate any field after Replace hands it to the store.
type Snapshot struct {
	ResourceName string
	Namespace    string
	EngineName   string
	EngineUID    types.UID
	RuleSetHash  string
	Version      string // "sha256:<64hex>"
	Resource     *corev3.TypedExtensionConfig
}

// ResourceName returns the ECDS resource name for an Engine. The naming
// contract is owned by internal/ecds/render, which serves the name; xds
// delegates to it because the same string is also the authorization key, and
// two implementations could drift apart and fail every Gateway closed.
func ResourceName(namespace, engineName string, uid types.UID) string {
	return render.ResourceName(namespace, engineName, uid)
}
