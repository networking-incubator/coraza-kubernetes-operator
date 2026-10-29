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
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

// The shared authorization/cache key must remain exactly namespace/name.
func TestNamespacedNameKey(t *testing.T) {
	const namespace, name = "shop", "checkout"
	want := types.NamespacedName{Namespace: namespace, Name: name}.String()
	if got := namespacedNameKey(namespace, name); got != want || got != "shop/checkout" {
		t.Fatalf("namespacedNameKey = %q, want %q (shop/checkout)", got, want)
	}
	if got := namespacedNameKey("", name); got != "/checkout" {
		t.Fatalf("empty namespace key = %q, want /checkout", got)
	}
}

// The name xds authorizes on must be the name render serves; a drift would
// fail every subscription closed with no error anywhere.
func TestResourceNameMatchesRender(t *testing.T) {
	cases := []struct{ namespace, engine string }{
		{"shop", "checkout"},
		{"x.a", "b"},
		{"x", "a.b"},
	}
	for _, c := range cases {
		uid := types.UID("engine-uid")
		if got, want := ResourceName(c.namespace, c.engine, uid), render.ResourceName(c.namespace, c.engine, uid); got != want {
			t.Fatalf("ResourceName(%q, %q) = %q, want %q", c.namespace, c.engine, got, want)
		}
	}
}
