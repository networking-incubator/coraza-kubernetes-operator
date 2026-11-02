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

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	wasmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

func TestSnapshotStoreLifecycle(t *testing.T) {
	store := NewSnapshotStore()

	require.NoError(t, store.Replace(&Snapshot{
		Namespace:  "ns",
		EngineName: "engine",
		Version:    "v1",
		Resource:   &corev3.TypedExtensionConfig{Name: "resource"},
	}))

	snapshot, ok := store.Get("ns/engine")
	require.True(t, ok)
	assert.Equal(t, "v1", snapshot.Version)

	store.DeleteEngine("ns", "engine")
	_, ok = store.Get("ns/engine")
	assert.False(t, ok)
}

func TestSnapshotStoreBumpKeepsAcceptedVersion(t *testing.T) {
	store := NewSnapshotStore()
	wasm := &wasmhttp.Wasm{Config: &wasmv3.PluginConfig{
		Name: "coraza.waf.ns.engine.uid",
		Vm: &wasmv3.PluginConfig_VmConfig{VmConfig: &wasmv3.VmConfig{
			VmId:                "coraza-uid",
			NackOnCodeCacheMiss: true,
			Code: &corev3.AsyncDataSource{Specifier: &corev3.AsyncDataSource_Remote{
				Remote: &corev3.RemoteDataSource{HttpUri: &corev3.HttpUri{Uri: "https://operator/wasm/plugin.wasm"}, Sha256: "digest"},
			}},
		}},
	}}
	typed, err := anypb.New(wasm)
	require.NoError(t, err)
	original := &corev3.TypedExtensionConfig{Name: "coraza.waf.ns.engine.uid", TypedConfig: typed}
	require.NoError(t, store.Replace(&Snapshot{
		ResourceName: "coraza.waf.ns.engine.uid",
		Namespace:    "ns",
		EngineName:   "engine",
		Version:      "sha256:v1",
		Resource:     original,
	}))

	require.NoError(t, store.BumpRejected("coraza.waf.ns.engine.uid", "sha256:v1", 1))

	cached, err := store.cache.GetSnapshot("ns/engine")
	require.NoError(t, err)
	assert.Equal(t, "sha256:v1.retry1", cached.GetVersion(resource.ExtensionConfigType))
	retried := cached.GetResources(resource.ExtensionConfigType)[original.Name].(*corev3.TypedExtensionConfig)
	assert.False(t, proto.Equal(original.TypedConfig, retried.TypedConfig), "Envoy skips identical typed configs even after a NACK")
	var retryWasm wasmhttp.Wasm
	require.NoError(t, retried.TypedConfig.UnmarshalTo(&retryWasm))
	assert.Equal(t, "coraza-uid.retry1", retryWasm.Config.GetVmConfig().VmId)
	retryWasm.Config.GetVmConfig().VmId = "coraza-uid"
	assert.True(t, proto.Equal(wasm, &retryWasm), "retry must preserve the remote source and plugin settings")
	var acceptedWasm wasmhttp.Wasm
	require.NoError(t, original.TypedConfig.UnmarshalTo(&acceptedWasm))
	assert.True(t, proto.Equal(wasm, &acceptedWasm), "retry must not mutate the accepted resource")
	assert.True(t, proto.Equal(original, store.meta["ns/engine"].Resource), "the accepted resource must remain unchanged")

	snapshot, ok := store.Get("ns/engine")
	require.True(t, ok)
	assert.Equal(t, "sha256:v1", snapshot.Version, "the accepted version must not change")
	assert.Error(t, store.BumpRejected("missing", "sha256:v1", 1))
}

func TestSnapshotStoreRejectsEmptySnapshot(t *testing.T) {
	store := NewSnapshotStore()
	assert.Error(t, store.Replace(nil))
	assert.Error(t, store.Replace(&Snapshot{Version: "v1"}))
}

func TestSnapshotStoreBumpSkipsStaleRetry(t *testing.T) {
	store := NewSnapshotStore()
	resourceName := "coraza.waf.ns.engine.uid"
	original := &corev3.TypedExtensionConfig{Name: resourceName, TypedConfig: mustWasm(t, "coraza-uid")}
	require.NoError(t, store.Replace(&Snapshot{
		ResourceName: resourceName,
		Namespace:    "ns",
		EngineName:   "engine",
		Version:      "sha256:v1",
		Resource:     original,
	}))

	// A newer accepted configuration replaced the rejected one before the retry
	// timer fired. The retry must not publish the older content over it.
	require.NoError(t, store.Replace(&Snapshot{
		ResourceName: resourceName,
		Namespace:    "ns",
		EngineName:   "engine",
		Version:      "sha256:v2",
		Resource:     &corev3.TypedExtensionConfig{Name: resourceName, TypedConfig: mustWasm(t, "coraza-uid-v2")},
	}))
	assert.Error(t, store.BumpRejected(resourceName, "sha256:v1", 1))

	cached, err := store.cache.GetSnapshot("ns/engine")
	require.NoError(t, err)
	assert.Equal(t, "sha256:v2", cached.GetVersion(resource.ExtensionConfigType))
	meta, ok := store.Get("ns/engine")
	require.True(t, ok)
	assert.Equal(t, "sha256:v2", meta.Version)
}

func mustWasm(t *testing.T, vmID string) *anypb.Any {
	t.Helper()
	typed, err := anypb.New(&wasmhttp.Wasm{Config: &wasmv3.PluginConfig{
		Vm: &wasmv3.PluginConfig_VmConfig{VmConfig: &wasmv3.VmConfig{VmId: vmID}},
	}})
	require.NoError(t, err)
	return typed
}

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
