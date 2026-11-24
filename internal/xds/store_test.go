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
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	wasmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	stream "github.com/envoyproxy/go-control-plane/pkg/server/stream/v3"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

const (
	storeEngineKey     = "ns/engine"
	storeResourceName  = "coraza.waf.ns.engine.uid"
	otherProcessEpoch  = "0123456789abcdef"
	storeBaseVMID      = "coraza-uid"
	storeContentPrefix = "sha256:"
)

// engineSnapshot is the published configuration of an Engine with version.
func engineSnapshot(t *testing.T, version string) *Snapshot {
	t.Helper()
	return &Snapshot{
		ResourceName: storeResourceName,
		Namespace:    "ns",
		EngineName:   "engine",
		Version:      storeContentPrefix + version,
		Resource:     &corev3.TypedExtensionConfig{Name: storeResourceName, TypedConfig: mustWasm(t, storeBaseVMID)},
	}
}

// publishedStore is a store holding the Engine's configuration with version v1.
func publishedStore(t *testing.T) *SnapshotStore {
	t.Helper()
	store := NewSnapshotStore()
	require.NoError(t, store.Replace(engineSnapshot(t, "v1")))
	return store
}

// servedAt is what the cache sends for a node: the xDS version and the resource,
// marshaled as go-control-plane marshals it for a response.
func servedAt(t *testing.T, store *SnapshotStore, node string) (string, []byte) {
	t.Helper()
	cached, err := store.cache.GetSnapshot(node)
	require.NoError(t, err)
	held := cached.GetResources(resource.ExtensionConfigType)
	require.Len(t, held, 1)
	marshaled, err := cachev3.MarshalResource(slices.Collect(maps.Values(held))[0])
	require.NoError(t, err)
	return cached.GetVersion(resource.ExtensionConfigType), marshaled
}

// servedWasm is the resource name and the WASM filter of a marshaled resource.
func servedWasm(t *testing.T, marshaled []byte) (string, *wasmhttp.Wasm) {
	t.Helper()
	var served corev3.TypedExtensionConfig
	require.NoError(t, proto.Unmarshal(marshaled, &served))
	var wasm wasmhttp.Wasm
	require.NoError(t, served.TypedConfig.UnmarshalTo(&wasm))
	return served.GetName(), &wasm
}

// vmIDOf is the VM ID of a marshaled resource.
func vmIDOf(t *testing.T, marshaled []byte) string {
	t.Helper()
	_, wasm := servedWasm(t, marshaled)
	return wasm.GetConfig().GetVmConfig().GetVmId()
}

// servedVersionAndVMID is the xDS version and the VM ID the cache sends for a
// lease.
func servedVersionAndVMID(t *testing.T, store *SnapshotStore, lease tagLease) (string, string) {
	t.Helper()
	version, marshaled := servedAt(t, store, lease.node())
	return version, vmIDOf(t, marshaled)
}

// heldTags is how many tag snapshots the Engine holds.
func heldTags(store *SnapshotStore) int {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return len(store.engines[storeEngineKey].tags)
}

func mustAcquire(t *testing.T, store *SnapshotStore, claim string) tagLease {
	t.Helper()
	lease, err := store.Acquire(storeEngineKey, claim)
	require.NoError(t, err)
	return lease
}

// holdForeignTags holds n tags of another process, as proxies that built them
// report them, and returns the leases.
func holdForeignTags(t *testing.T, store *SnapshotStore, version string, n int) []tagLease {
	t.Helper()
	leases := make([]tagLease, 0, n)
	for gen := range n {
		leases = append(leases, mustAcquire(t, store, wireVersion(version, tag{epoch: otherProcessEpoch, gen: uint64(gen)})))
	}
	return leases
}

func TestSnapshotStoreLifecycle(t *testing.T) {
	store := publishedStore(t)

	snapshot, ok := store.Get(storeEngineKey)
	require.True(t, ok)
	assert.Equal(t, "sha256:v1", snapshot.Version)

	// A tag snapshot exists while a stream holds the tag.
	lease := mustAcquire(t, store, "")
	_, err := store.cache.GetSnapshot(lease.node())
	require.NoError(t, err)

	store.DeleteEngine("ns", "engine")
	_, ok = store.Get(storeEngineKey)
	assert.False(t, ok)
	_, err = store.cache.GetSnapshot(lease.node())
	assert.Error(t, err)
}

func TestSnapshotStoreRejectsEmptySnapshot(t *testing.T) {
	store := NewSnapshotStore()
	assert.Error(t, store.Replace(nil))
	assert.Error(t, store.Replace(&Snapshot{Version: "v1"}))
}

// Every resource is served under a tag, so a configuration can always be
// replaced by other bytes; the published snapshot never carries it.
func TestSnapshotStoreServesTheTagInVersionAndVMID(t *testing.T) {
	store := NewSnapshotStore()
	snapshot := engineSnapshot(t, "v1")
	require.NoError(t, store.Replace(snapshot))

	lease := mustAcquire(t, store, "")
	current := tag{epoch: store.epoch}
	require.Equal(t, current, lease.tag)
	version, vmID := servedVersionAndVMID(t, store, lease)
	assert.Equal(t, "sha256:v1."+current.String(), version)
	assert.Equal(t, storeBaseVMID+"."+current.String(), vmID)

	// Only the VM ID differs from the published resource.
	_, marshaled := servedAt(t, store, lease.node())
	name, wasm := servedWasm(t, marshaled)
	assert.Equal(t, storeResourceName, name)
	wasm.GetConfig().GetVmConfig().VmId = storeBaseVMID
	var published wasmhttp.Wasm
	require.NoError(t, snapshot.Resource.TypedConfig.UnmarshalTo(&published))
	assert.True(t, proto.Equal(&published, wasm))

	got, ok := store.Get(storeEngineKey)
	require.True(t, ok)
	assert.Equal(t, "sha256:v1", got.Version, "the content version never carries a tag")
	assert.Nil(t, got.Resource, "the store keeps the configuration once, not with each snapshot")
}

// A tag always renders to the same bytes, in any process: a proxy that moves to
// another replica, or finds the operator restarted, is sent the bytes it built
// and does not build its filter again.
func TestSnapshotStoreRendersATagToTheSameBytesInEveryProcess(t *testing.T) {
	elsewhere := tag{epoch: otherProcessEpoch, gen: 7}
	claim := wireVersion("sha256:v1", elsewhere)
	first, second := publishedStore(t), publishedStore(t)
	require.NotEqual(t, first.epoch, second.epoch)

	versionA, bytesA := servedAt(t, first, mustAcquire(t, first, claim).node())
	versionB, bytesB := servedAt(t, second, mustAcquire(t, second, claim).node())
	assert.Equal(t, claim, versionA)
	assert.Equal(t, versionA, versionB)
	assert.Equal(t, bytesA, bytesB)

	_, again := servedAt(t, first, mustAcquire(t, first, claim).node())
	assert.Equal(t, bytesA, again, "a snapshot is rendered the same each time it is sent")

	_, current := servedAt(t, first, mustAcquire(t, first, "").node())
	assert.NotEqual(t, bytesA, current, "another tag is other bytes")
}

func TestSnapshotStoreTagsAreUniquePerProcess(t *testing.T) {
	first, second := publishedStore(t), publishedStore(t)
	require.NotEqual(t, first.epoch, second.epoch)

	versionA, vmIDA := servedVersionAndVMID(t, first, mustAcquire(t, first, ""))
	versionB, vmIDB := servedVersionAndVMID(t, second, mustAcquire(t, second, ""))
	assert.NotEqual(t, versionA, versionB)
	assert.NotEqual(t, vmIDA, vmIDB)
}

func TestSnapshotStoreAdvanceGivesOtherBytes(t *testing.T) {
	store := publishedStore(t)
	first := mustAcquire(t, store, "")

	assert.False(t, store.Advance(storeEngineKey, tag{epoch: otherProcessEpoch}), "another process's tag is not the current one")
	assert.True(t, store.Advance(storeEngineKey, first.tag))
	assert.False(t, store.Advance(storeEngineKey, first.tag), "proxies that reject the same tag advance it once")

	second := mustAcquire(t, store, "")
	assert.Equal(t, tag{epoch: store.epoch, gen: 1}, second.tag)
	versionA, vmIDA := servedVersionAndVMID(t, store, first)
	versionB, vmIDB := servedVersionAndVMID(t, store, second)
	assert.NotEqual(t, versionA, versionB)
	assert.NotEqual(t, vmIDA, vmIDB)
	assert.Equal(t, 2, heldTags(store), "the stream on the old tag keeps it")

	store.Release(first)
	assert.Equal(t, 1, heldTags(store), "the old tag is dropped with its last lease")
	assert.False(t, store.Advance("ns/missing", first.tag))
}

func TestSnapshotStoreIsCurrent(t *testing.T) {
	store := publishedStore(t)
	current := tag{epoch: store.epoch}
	assert.True(t, store.isCurrent(storeEngineKey, current))
	assert.False(t, store.isCurrent(storeEngineKey, tag{epoch: otherProcessEpoch}), "a tag of another process")
	assert.False(t, store.isCurrent("ns/missing", current), "an Engine that is not there")

	require.True(t, store.Advance(storeEngineKey, current))
	assert.False(t, store.isCurrent(storeEngineKey, current), "a tag the Engine moved past")
	assert.True(t, store.isCurrent(storeEngineKey, tag{epoch: store.epoch, gen: 1}))
}

func TestParseWireVersion(t *testing.T) {
	const epoch = "0123456789abcdef"
	tests := []struct {
		name    string
		wire    string
		want    tag
		wantVer string
		ok      bool
	}{
		{name: "a tagged version", wire: "sha256:v1." + epoch + "-3", want: tag{epoch, 3}, wantVer: "sha256:v1", ok: true},
		{name: "generation zero", wire: "sha256:v1." + epoch + "-0", want: tag{epoch, 0}, wantVer: "sha256:v1", ok: true},
		{name: "the largest generation", wire: "sha256:v1." + epoch + "-18446744073709551615", want: tag{epoch, 18446744073709551615}, wantVer: "sha256:v1", ok: true},
		{name: "empty"},
		{name: "a bare content version", wire: "sha256:v1"},
		{name: "a retry version of an earlier release", wire: "sha256:v1.retry2"},
		{name: "no content version", wire: "." + epoch + "-3"},
		{name: "uppercase epoch", wire: "sha256:v1.0123456789ABCDEF-3"},
		{name: "short epoch", wire: "sha256:v1.0123456789abcde-3"},
		{name: "long epoch", wire: "sha256:v1.0123456789abcdef0-3"},
		{name: "epoch that is not hexadecimal", wire: "sha256:v1.0123456789abcdeg-3"},
		{name: "no generation", wire: "sha256:v1." + epoch + "-"},
		{name: "generation with a sign", wire: "sha256:v1." + epoch + "-+3"},
		{name: "generation with a leading zero", wire: "sha256:v1." + epoch + "-03"},
		{name: "generation with other characters", wire: "sha256:v1." + epoch + "-3x"},
		{name: "generation beyond 64 bits", wire: "sha256:v1." + epoch + "-18446744073709551616"},
		{name: "a second generation", wire: "sha256:v1." + epoch + "-3-4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			version, got, ok := parseWireVersion(tt.wire)
			require.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.wantVer, version)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestSnapshotStoreHonoursClaims(t *testing.T) {
	store := publishedStore(t)
	current := tag{epoch: store.epoch}
	elsewhere := tag{epoch: otherProcessEpoch, gen: 4}
	tests := []struct {
		name        string
		claim       string
		want        tag
		wantClaimed bool
	}{
		{name: "nothing claimed", want: current},
		{name: "the current tag", claim: wireVersion("sha256:v1", current), want: current, wantClaimed: true},
		{name: "a tag another process sent", claim: wireVersion("sha256:v1", elsewhere), want: elsewhere, wantClaimed: true},
		{name: "a tag this process never sent", claim: wireVersion("sha256:v1", tag{epoch: store.epoch, gen: 5}), want: current},
		{name: "a tag of other content", claim: wireVersion("sha256:v0", elsewhere), want: current},
		{name: "a bare content version", claim: "sha256:v1", want: current},
		{name: "a retry version of an earlier release", claim: "sha256:v1.retry3", want: current},
		{name: "an epoch that is not canonical", claim: "sha256:v1.0123456789ABCDEF-4", want: current},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lease := mustAcquire(t, store, tt.claim)
			t.Cleanup(func() { store.Release(lease) })
			assert.Equal(t, tt.want, lease.tag)
			assert.Equal(t, tt.wantClaimed, lease.claimed)
		})
	}
}

// A claim of the current content is honoured however many other tags the Engine
// holds: a proxy that built those bytes is sent them again and keeps its filter.
// A held tag costs a few words, so the claims of proxies cannot pin memory, and
// giving the leases back frees them.
func TestSnapshotStoreHonoursEveryClaimOfTheCurrentContent(t *testing.T) {
	store := publishedStore(t)
	const claims = 1000
	leases := holdForeignTags(t, store, "sha256:v1", claims)
	for gen, lease := range leases {
		assert.Equal(t, tag{epoch: otherProcessEpoch, gen: uint64(gen)}, lease.tag, "claim %d", gen)
		assert.True(t, lease.claimed, "claim %d", gen)
	}
	assert.Equal(t, claims, heldTags(store))

	// A tag that is held is shared, not counted again.
	leases = append(leases, mustAcquire(t, store, wireVersion("sha256:v1", tag{epoch: otherProcessEpoch})))
	assert.Equal(t, claims, heldTags(store))

	for _, lease := range leases {
		store.Release(lease)
	}
	assert.Zero(t, heldTags(store))
	for _, lease := range leases {
		_, err := store.cache.GetSnapshot(lease.node())
		assert.Error(t, err, "tag %s is dropped", lease.tag)
	}
}

// A claim of a generation this process has not reached is refused, however it
// is spelt: the Engine would later reach it, and send a proxy the bytes it
// reported having built, or rejected.
func TestSnapshotStoreRefusesGenerationsItHasNotReached(t *testing.T) {
	store := publishedStore(t)
	current := tag{epoch: store.epoch}
	for _, gen := range []uint64{1, 2, 1 << 32, 1<<64 - 1} {
		lease := mustAcquire(t, store, wireVersion("sha256:v1", tag{epoch: store.epoch, gen: gen}))
		assert.Equal(t, current, lease.tag, "generation %d", gen)
		assert.False(t, lease.claimed)
	}
	store.Advance(storeEngineKey, current)
	lease := mustAcquire(t, store, wireVersion("sha256:v1", current))
	assert.Equal(t, current, lease.tag, "a generation the Engine has left is still its own")
	assert.True(t, lease.claimed)
}

func TestSnapshotStoreReleasesReferences(t *testing.T) {
	store := publishedStore(t)
	elsewhere := tag{epoch: otherProcessEpoch, gen: 1}
	claim := wireVersion("sha256:v1", elsewhere)

	first, second := mustAcquire(t, store, claim), mustAcquire(t, store, claim)
	require.Equal(t, elsewhere, first.tag)
	store.Release(first)
	_, err := store.cache.GetSnapshot(second.node())
	assert.NoError(t, err, "a tag stays while a stream holds it")
	store.Release(second)
	_, err = store.cache.GetSnapshot(second.node())
	assert.Error(t, err, "a tag nobody holds is dropped")

	// The current tag is dropped with its last stream as well: it is cheap to
	// serve again.
	current := mustAcquire(t, store, "")
	store.Release(current)
	_, err = store.cache.GetSnapshot(current.node())
	assert.Error(t, err)
	assert.Zero(t, heldTags(store))
	next := mustAcquire(t, store, "")
	_, err = store.cache.GetSnapshot(next.node())
	assert.NoError(t, err)
	store.Release(next)

	// A lease of a deleted Engine does not release the snapshot of its successor.
	old := mustAcquire(t, store, "")
	store.DeleteEngine("ns", "engine")
	require.NoError(t, store.Replace(engineSnapshot(t, "v1")))
	fresh := mustAcquire(t, store, "")
	store.Release(old)
	_, err = store.cache.GetSnapshot(fresh.node())
	assert.NoError(t, err)
	assert.Equal(t, 1, store.engines[storeEngineKey].tags[fresh.heldTag])
}

// A snapshot that carries the bytes already published only refreshes what is
// recorded about it: the tag snapshots are not built again.
func TestSnapshotStoreKeepsItsSnapshotsForUnchangedContent(t *testing.T) {
	store := publishedStore(t)
	held := mustAcquire(t, store, wireVersion("sha256:v1", tag{epoch: otherProcessEpoch, gen: 2}))
	before, err := store.cache.GetSnapshot(held.node())
	require.NoError(t, err)

	same := engineSnapshot(t, "v1")
	same.RuleSetHash = "sha256:other-inputs"
	require.NoError(t, store.Replace(same))
	after, err := store.cache.GetSnapshot(held.node())
	require.NoError(t, err)
	assert.Same(t, before, after)
	got, ok := store.Get(storeEngineKey)
	require.True(t, ok)
	assert.Equal(t, "sha256:other-inputs", got.RuleSetHash)

	require.NoError(t, store.Replace(engineSnapshot(t, "v2")))
	rebuilt, err := store.cache.GetSnapshot(held.node())
	require.NoError(t, err)
	assert.NotSame(t, before, rebuilt, "new content is built")
}

// New content reaches every tag the Engine holds: the tags this process minted
// carry it under themselves, and a tag another process minted carries it under
// this process's current tag, for that process may send the same tag with the same
// bytes. A claim for the old content is no longer honoured.
func TestSnapshotStoreRebuildsHeldTagsForNewContent(t *testing.T) {
	store := publishedStore(t)
	elsewhere := tag{epoch: otherProcessEpoch, gen: 2}
	held := mustAcquire(t, store, wireVersion("sha256:v1", elsewhere))
	require.Equal(t, elsewhere, held.tag)
	earlier := mustAcquire(t, store, "")
	require.True(t, store.Advance(storeEngineKey, earlier.tag))
	current := mustAcquire(t, store, "")
	require.Equal(t, tag{epoch: store.epoch, gen: 1}, current.tag)

	require.NoError(t, store.Replace(engineSnapshot(t, "v2")))
	version, vmID := servedVersionAndVMID(t, store, held)
	assert.Equal(t, wireVersion("sha256:v2", current.tag), version, "a tag of another process is sent the current tag")
	assert.Equal(t, storeBaseVMID+"."+current.tag.String(), vmID)
	version, vmID = servedVersionAndVMID(t, store, earlier)
	assert.Equal(t, wireVersion("sha256:v2", earlier.tag), version, "a tag of this process stays its own")
	assert.Equal(t, storeBaseVMID+"."+earlier.tag.String(), vmID)
	version, _ = servedVersionAndVMID(t, store, current)
	assert.Equal(t, wireVersion("sha256:v2", current.tag), version)

	stale := mustAcquire(t, store, wireVersion("sha256:v1", elsewhere))
	assert.Equal(t, current.tag, stale.tag)
	assert.False(t, stale.claimed)
}

// A proxy that moves to another replica, which replays the bytes it built under
// the tag of the replica it came from, is not sent new content under that tag.
// The replica it came from would send the same tag with the same bytes if the
// proxy returned, and Envoy accepts bytes it already received without building
// them if it rejected them on the other. A proxy that reports the tag for the new
// content is replayed exactly those bytes, from a snapshot of its own, whichever
// proxies still hold the tag from before the publication.
func TestSnapshotStoreNeverSendsNewContentUnderAnotherProcessTag(t *testing.T) {
	origin, replica := publishedStore(t), publishedStore(t)
	elsewhere := tag{epoch: origin.epoch}
	claim := wireVersion("sha256:v1", elsewhere)

	// The proxy is replayed the bytes the origin sent it.
	held := mustAcquire(t, replica, claim)
	require.Equal(t, elsewhere, held.tag)
	require.True(t, held.claimed)
	inOrigin := mustAcquire(t, origin, "")
	_, sent := servedAt(t, origin, inOrigin.node())
	_, replayed := servedAt(t, replica, held.node())
	assert.Equal(t, sent, replayed)

	// New content published by both reaches the proxy from the replica under its
	// own tag, so the bytes the origin sends are others.
	require.NoError(t, replica.Replace(engineSnapshot(t, "v2")))
	require.NoError(t, origin.Replace(engineSnapshot(t, "v2")))
	version, fromReplica := servedAt(t, replica, held.node())
	assert.Equal(t, wireVersion("sha256:v2", tag{epoch: replica.epoch}), version)
	versionOrigin, fromOrigin := servedAt(t, origin, inOrigin.node())
	assert.Equal(t, wireVersion("sha256:v2", elsewhere), versionOrigin)
	assert.NotEqual(t, fromOrigin, fromReplica)

	// A proxy that built the origin's bytes of the new content is replayed those.
	other := mustAcquire(t, replica, wireVersion("sha256:v2", elsewhere))
	require.True(t, other.claimed)
	require.Equal(t, elsewhere, other.tag)
	assert.NotEqual(t, held.node(), other.node())
	versionOther, replayedNew := servedAt(t, replica, other.node())
	assert.Equal(t, wireVersion("sha256:v2", elsewhere), versionOther)
	assert.Equal(t, fromOrigin, replayedNew)
	_, stillSent := servedAt(t, replica, held.node())
	assert.Equal(t, fromReplica, stillSent, "the holders for the old content keep the bytes they were moved to")

	// Proxies that report the tag for the same content share a snapshot.
	again := mustAcquire(t, replica, wireVersion("sha256:v2", elsewhere))
	assert.Equal(t, other.node(), again.node())
	assert.Equal(t, 2, heldTags(replica))
}

// A proxy that reports the tag of another process for content that returns to a
// version it was held for earlier is still replayed exactly those bytes, from a
// snapshot of its own: the one held since, which was sent this process's tag, is
// not shared.
func TestSnapshotStoreReplaysAClaimWhenContentReturns(t *testing.T) {
	origin, replica := publishedStore(t), publishedStore(t)
	elsewhere := tag{epoch: origin.epoch}
	claim := wireVersion("sha256:v1", elsewhere)
	held := mustAcquire(t, replica, claim)
	require.True(t, held.claimed)

	require.NoError(t, replica.Replace(engineSnapshot(t, "v2")))
	require.NoError(t, replica.Replace(engineSnapshot(t, "v1")))
	again := mustAcquire(t, replica, claim)
	require.True(t, again.claimed)
	assert.NotEqual(t, held.node(), again.node())
	_, replayed := servedAt(t, replica, again.node())
	_, sent := servedAt(t, origin, mustAcquire(t, origin, "").node())
	assert.Equal(t, sent, replayed)
}

// A stream that arrives before the Engine is published holds a tag with no
// snapshot yet; publication fills it, so the stream is answered on its watch.
func TestSnapshotStoreServesAnEngineBeforeItIsPublished(t *testing.T) {
	store := NewSnapshotStore()
	lease := mustAcquire(t, store, wireVersion("sha256:v1", tag{epoch: otherProcessEpoch}))
	assert.Equal(t, tag{epoch: store.epoch}, lease.tag, "a claim cannot be checked before the content is known")
	_, ok := store.Get(storeEngineKey)
	assert.False(t, ok)
	_, err := store.cache.GetSnapshot(lease.node())
	assert.Error(t, err)

	require.NoError(t, store.Replace(engineSnapshot(t, "v1")))
	version, _ := servedVersionAndVMID(t, store, lease)
	assert.Equal(t, wireVersion("sha256:v1", lease.tag), version)

	empty := NewSnapshotStore()
	store.DeleteEngine("ns", "engine")
	store.Release(lease)
	waiting := mustAcquire(t, empty, "")
	empty.Release(waiting)
	assert.Empty(t, empty.engines, "an Engine nobody waits for or has published keeps no state")
}

func TestSnapshotStoreRejectsAResourceItCannotTag(t *testing.T) {
	for name, res := range map[string]*corev3.TypedExtensionConfig{
		"no typed config": {Name: storeResourceName},
		"no VM":           {Name: storeResourceName, TypedConfig: mustAny(t, &wasmhttp.Wasm{Config: &wasmv3.PluginConfig{}})},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := engineSnapshot(t, "v1")
			snapshot.Resource = res
			store := NewSnapshotStore()
			assert.Error(t, store.Replace(snapshot))
			_, ok := store.Get(storeEngineKey)
			assert.False(t, ok)
		})
	}
}

func mustAny(t *testing.T, message proto.Message) *anypb.Any {
	t.Helper()
	typed, err := anypb.New(message)
	require.NoError(t, err)
	return typed
}

// Streams waiting out a rejection are told about a version the Engine did not
// have, and only about that: the first publication and new content, not the same
// content again. The store's lock is released when they are told.
func TestSnapshotStoreNotifiesWhenTheVersionChanges(t *testing.T) {
	store := NewSnapshotStore()
	var notified []string
	store.setOnChange(func(engineKey string) {
		_, ok := store.Get(engineKey)
		assert.True(t, ok)
		notified = append(notified, engineKey)
	})

	for _, version := range []string{"v1", "v1", "v2"} {
		require.NoError(t, store.Replace(engineSnapshot(t, version)))
	}
	assert.Equal(t, []string{storeEngineKey, storeEngineKey}, notified)
}

// Streams, rejections and publications race on one Engine: every snapshot the
// store holds is released with the last lease.
func TestSnapshotStoreConcurrentUse(t *testing.T) {
	store := publishedStore(t)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 200 {
				claim := ""
				if i%3 == 0 {
					claim = wireVersion("sha256:v1", tag{epoch: otherProcessEpoch, gen: uint64((worker + i) % 12)})
				}
				lease, err := store.Acquire(storeEngineKey, claim)
				assert.NoError(t, err)
				if i%5 == 0 {
					store.Advance(storeEngineKey, lease.tag)
				}
				store.Release(lease)
			}
		})
	}
	wg.Go(func() {
		for i := range 20 {
			assert.NoError(t, store.Replace(engineSnapshot(t, fmt.Sprintf("v%d", i%2+1))))
		}
	})
	wg.Wait()

	assert.Zero(t, heldTags(store))
	store.mu.RLock()
	defer store.mu.RUnlock()
	for gen := range store.engines[storeEngineKey].gen + 1 {
		_, err := store.cache.GetSnapshot(tagNode(storeEngineKey, tag{epoch: store.epoch, gen: gen}))
		assert.Error(t, err, "generation %d", gen)
	}
	for gen := range 12 {
		for publications := range store.engines[storeEngineKey].renders + 1 {
			_, err := store.cache.GetSnapshot(heldTag{tag: tag{epoch: otherProcessEpoch, gen: uint64(gen)}, render: publications}.node(storeEngineKey))
			assert.Error(t, err, "generation %d of another process, held after %d publications", gen, publications)
		}
	}
}

// withConfiguration is snapshot carrying size bytes of plugin configuration, as
// the rules and data of an Engine give.
func withConfiguration(t *testing.T, snapshot *Snapshot, size int) *Snapshot {
	t.Helper()
	var wasm wasmhttp.Wasm
	require.NoError(t, snapshot.Resource.TypedConfig.UnmarshalTo(&wasm))
	wasm.Config.Configuration = mustAny(t, wrapperspb.String(strings.Repeat("r", size)))
	snapshot.Resource.TypedConfig = mustAny(t, &wasm)
	return snapshot
}

// respond asks the cache for a node's resource as a stream does, and marshals the
// response as the server does before it sends it.
func respond(t *testing.T, store *SnapshotStore, node string) *discoveryv3.DiscoveryResponse {
	t.Helper()
	responses := make(chan cachev3.Response, 1)
	_, err := store.cache.CreateWatch(
		&cachev3.Request{Node: &corev3.Node{Id: node}, TypeUrl: resource.ExtensionConfigType},
		stream.NewSotwSubscription(nil, false), responses)
	require.NoError(t, err)
	response, err := (<-responses).GetDiscoveryResponse()
	require.NoError(t, err)
	return response
}

// An Engine holds its configuration once, and a tag a stream holds adds a few
// words, also once the stream has been sent it: the bytes of a tag are produced
// when it is sent, and go-control-plane keeps what it marshals for the response
// only. Streams that report tags of their own, as proxies that were each rejected
// once do, and a client that claims tags to pin memory, cost no more than the
// streams that hold them.
func TestSnapshotStoreTagsHoldNoConfigurationBytes(t *testing.T) {
	for _, size := range []int{3 << 20, 8 << 20} {
		t.Run(fmt.Sprintf("%d MiB", size>>20), func(t *testing.T) {
			const tags, served = 200, 16
			before := heapBytes()
			store := NewSnapshotStore()
			require.NoError(t, store.Replace(withConfiguration(t, engineSnapshot(t, "v1"), size)))
			perEngine := heapGrowth(before, heapBytes())
			assert.Less(t, perEngine, uint64(size)*5/4, "the Engine holds the configuration once")
			assert.Greater(t, perEngine, uint64(size)/2, "the heap measure sees the configuration")

			leases := holdForeignTags(t, store, "sha256:v1", tags)
			for _, lease := range leases[:served] {
				respond(t, store, lease.node())
			}
			perTag := heapGrowth(before+perEngine, heapBytes()) / tags
			t.Logf("%d MiB configuration: %d KiB for the Engine, %d bytes for each of %d held tags, %d of them sent", size>>20, perEngine>>10, perTag, tags, served)
			assert.Less(t, perTag, uint64(16<<10), "a held tag is a few words")
			assert.Equal(t, tags, heldTags(store))

			// The bytes sent under a tag carry the whole configuration.
			response := respond(t, store, leases[tags-1].node())
			_, wasm := servedWasm(t, response.GetResources()[0].GetValue())
			var content wrapperspb.StringValue
			require.NoError(t, wasm.GetConfig().GetConfiguration().UnmarshalTo(&content))
			assert.Len(t, content.GetValue(), size)

			runtime.KeepAlive(store)
			runtime.KeepAlive(leases)
		})
	}
}

// Responses sent at the same time under one tag share one rendering of it, which
// nothing keeps once they are done.
func TestTagResourceSharesTheRenderingOfResponsesInFlight(t *testing.T) {
	published, err := newContent(engineSnapshot(t, "v1"), logr.Discard())
	require.NoError(t, err)
	held := &tagResource{content: published, tag: tag{epoch: otherProcessEpoch}}

	first, second := held.ProtoReflect(), held.ProtoReflect()
	assert.Same(t, first.Interface(), second.Interface())
	other := (&tagResource{content: published, tag: tag{epoch: otherProcessEpoch, gen: 1}}).ProtoReflect()
	assert.NotSame(t, first.Interface(), other.Interface(), "another tag is another resource")
	runtime.KeepAlive(first)
}

// logLines is a logger's output.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

// logger writes to the lines.
func (l *logLines) logger() logr.Logger {
	return funcr.New(func(_, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, args)
	}, funcr.Options{})
}

// with returns the lines that contain text.
func (l *logLines) with(text string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(l.lines), func(line string) bool { return !strings.Contains(line, text) })
}

// A rendering that fails, which what decoded does not, is logged once for the
// content however many tags and responses it spoils, and the proxy is sent a
// resource without a configuration, which it rejects.
func TestContentLogsAFailedRenderingOnce(t *testing.T) {
	var logs logLines
	published, err := newContent(engineSnapshot(t, "v1"), logs.logger())
	require.NoError(t, err)
	// A string that is not UTF-8 does not encode.
	published.template.GetConfig().GetVmConfig().Runtime = "\xff"

	for gen := range 3 {
		// Nothing holds the rendering between responses.
		rendered, ok := (&tagResource{content: published, tag: tag{epoch: otherProcessEpoch, gen: uint64(gen)}}).ProtoReflect().Interface().(*corev3.TypedExtensionConfig)
		require.True(t, ok)
		assert.Equal(t, storeResourceName, rendered.GetName())
		assert.Nil(t, rendered.GetTypedConfig())
	}
	assert.Len(t, logs.with("Failed to render ECDS resource"), 1)
}

// Publishing new content costs the configuration once, whatever the number of
// tags the Engine holds: each tag is a few words that follow the content.
func TestSnapshotStoreReplaceDoesNotScaleWithHeldTags(t *testing.T) {
	const size, tags = 8 << 20, 200
	allocated := func(store *SnapshotStore, version string) int64 {
		snapshot := withConfiguration(t, engineSnapshot(t, version), size)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		require.NoError(t, store.Replace(snapshot))
		runtime.ReadMemStats(&after)
		return int64(after.TotalAlloc - before.TotalAlloc)
	}
	store := publishedStore(t)
	alone := allocated(store, "v2")
	leases := holdForeignTags(t, store, "sha256:v2", tags)
	held := allocated(store, "v3")
	t.Logf("publishing %d MiB of new content allocates %d KiB with one tag held and %d KiB with %d", size>>20, alone>>10, held>>10, tags+1)
	assert.Less(t, held, alone+tags*(16<<10))
	assert.Less(t, held, int64(size)*3/2, "the content is decoded once")

	version, _ := servedVersionAndVMID(t, store, leases[tags-1])
	assert.Equal(t, wireVersion("sha256:v3", tag{epoch: store.epoch}), version, "every held tag follows the content")
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
