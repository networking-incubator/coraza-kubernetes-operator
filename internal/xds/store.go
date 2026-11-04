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
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

// SnapshotStore publishes one shared ECDS snapshot per Engine and isolated
// retry snapshots per proxy into go-control-plane's SnapshotCache.
type SnapshotStore struct {
	cache cachev3.SnapshotCache

	mu      sync.RWMutex
	meta    map[string]Snapshot
	proxies map[string]*proxySnapshot
	// byEngine indexes proxies per Engine key so eviction and Replace touch
	// only that Engine's proxies instead of scanning the whole store.
	byEngine map[string]map[string]*proxySnapshot
	// resources indexes ECDS resource name to Engine key so the request path
	// resolves a subscription without scanning every Engine.
	resources map[string]string
	nextSeq   uint64
}

type proxySnapshot struct {
	engineKey    string
	active       int
	retryUnacked bool
	// seq orders inactive retry snapshots for eviction.
	seq uint64
}

// maxInactiveRetryProxies bounds how many disconnected, unacknowledged retry
// snapshots one Engine retains. A subscriber that NACKs under distinct proxy
// keys and never returns must not grow the store without bound. An evicted
// proxy is not stranded: on its next NACK the server falls back to the
// Engine's shared version (CurrentVersionFor) and the retry ladder restarts,
// exactly like a proxy whose retry was purged by a content change.
const maxInactiveRetryProxies = 32

// NewSnapshotStore returns an empty snapshot store.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{
		cache:     cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil),
		meta:      make(map[string]Snapshot),
		proxies:   make(map[string]*proxySnapshot),
		byEngine:  make(map[string]map[string]*proxySnapshot),
		resources: make(map[string]string),
	}
}

// trackProxyLocked registers a proxy in the global and per-Engine indexes.
func (s *SnapshotStore) trackProxyLocked(proxyKey string, proxy *proxySnapshot) {
	s.proxies[proxyKey] = proxy
	engine := s.byEngine[proxy.engineKey]
	if engine == nil {
		engine = make(map[string]*proxySnapshot)
		s.byEngine[proxy.engineKey] = engine
	}
	engine[proxyKey] = proxy
}

// untrackProxyLocked removes a proxy from both indexes.
func (s *SnapshotStore) untrackProxyLocked(proxyKey string) {
	proxy, ok := s.proxies[proxyKey]
	if !ok {
		return
	}
	delete(s.proxies, proxyKey)
	engine := s.byEngine[proxy.engineKey]
	if engine == nil {
		return
	}
	delete(engine, proxyKey)
	if len(engine) == 0 {
		delete(s.byEngine, proxy.engineKey)
	}
}

// Replace publishes an Engine snapshot. The cache key is the Engine key, not
// the ECDS resource name, so it survives a UID change and can be deleted by
// namespace/name.
func (s *SnapshotStore) Replace(snapshot *Snapshot) error {
	if snapshot == nil || snapshot.Resource == nil {
		return errors.New("snapshot and resource are required")
	}
	compiled, err := cachev3.NewSnapshot(snapshot.Version, map[resource.Type][]cachetypes.Resource{
		resource.ExtensionConfigType: {snapshot.Resource},
	})
	if err != nil {
		return fmt.Errorf("build snapshot: %w", err)
	}
	key := namespacedNameKey(snapshot.Namespace, snapshot.EngineName)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cache.SetSnapshot(context.Background(), key, compiled); err != nil {
		return err
	}
	// A UID change republishes under a new resource name; drop the old index
	// entry so it cannot shadow another Engine's name.
	if old, ok := s.meta[key]; ok && old.ResourceName != snapshot.ResourceName {
		delete(s.resources, old.ResourceName)
	}
	s.meta[key] = *snapshot
	s.resources[snapshot.ResourceName] = key
	for proxyKey, proxy := range s.byEngine[key] {
		proxy.retryUnacked = false
		if proxy.active == 0 {
			s.cache.ClearSnapshot(proxyKey)
			s.untrackProxyLocked(proxyKey)
			continue
		}
		if err := s.cache.SetSnapshot(context.Background(), proxyKey, compiled); err != nil {
			return fmt.Errorf("update proxy snapshot %q: %w", proxyKey, err)
		}
	}
	return nil
}

// BumpRejected publishes a derived snapshot for one proxy so Envoy re-applies a
// rejected typed configuration without changing the Engine's shared snapshot.
// acceptedVersion must still be the current snapshot version; a concurrent
// Replace with newer content makes the retry stale and it is skipped.
func (s *SnapshotStore) BumpRejected(resourceName, proxyKey, acceptedVersion string, attempt int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key, meta, ok := s.snapshotForResourceLocked(resourceName)
	if !ok {
		return fmt.Errorf("no snapshot for resource %q", resourceName)
	}
	if proxyKey == "" || proxyKey == key {
		return errors.New("a distinct proxy cache key is required")
	}
	// An empty version cannot identify the rejected snapshot (for example a
	// first-message NACK); retrying the current snapshot is safe because it
	// cannot restore older content.
	if acceptedVersion != "" && meta.Version != acceptedVersion {
		return fmt.Errorf("snapshot for resource %q advanced from %q to %q", resourceName, acceptedVersion, meta.Version)
	}
	proxy := s.proxies[proxyKey]
	if proxy != nil && proxy.engineKey != key {
		return fmt.Errorf("proxy cache key %q belongs to another Engine", proxyKey)
	}

	compiled, err := rejectedSnapshot(meta, attempt)
	if err != nil {
		return err
	}
	if err := s.cache.SetSnapshot(context.Background(), proxyKey, compiled); err != nil {
		return err
	}
	if proxy == nil {
		proxy = &proxySnapshot{engineKey: key, seq: s.nextSeq}
		s.nextSeq++
		s.trackProxyLocked(proxyKey, proxy)
	} else {
		// A repeated NACK refreshes the proxy's eviction order, so a proxy
		// that keeps rejecting is never evicted as old while it is the
		// active troublemaker.
		proxy.seq = s.nextSeq
		s.nextSeq++
	}
	proxy.retryUnacked = true
	s.evictInactiveRetriesLocked(key)
	return nil
}

// evictInactiveRetriesLocked drops the oldest disconnected, unacknowledged
// retry snapshots of one Engine beyond maxInactiveRetryProxies. Connected
// proxies and recent disconnects keep their snapshots; an evicted proxy
// restarts the retry ladder on its next NACK, which is why the eviction is
// safe. Only that Engine's proxies are examined, so a NACK does not scan the
// whole store.
func (s *SnapshotStore) evictInactiveRetriesLocked(engineKey string) {
	engine := s.byEngine[engineKey]
	if engine == nil {
		return
	}
	retained := 0
	candidates := make([]string, 0, len(engine))
	for proxyKey, proxy := range engine {
		if proxy.active > 0 || !proxy.retryUnacked {
			continue
		}
		retained++
		candidates = append(candidates, proxyKey)
	}
	if retained <= maxInactiveRetryProxies {
		return
	}
	sort.Slice(candidates, func(i, j int) bool {
		return engine[candidates[i]].seq < engine[candidates[j]].seq
	})
	for _, proxyKey := range candidates[:retained-maxInactiveRetryProxies] {
		s.cache.ClearSnapshot(proxyKey)
		s.untrackProxyLocked(proxyKey)
	}
}

func rejectedSnapshot(meta Snapshot, attempt int) (cachev3.ResourceSnapshot, error) {
	retried := proto.Clone(meta.Resource).(*corev3.TypedExtensionConfig)
	var wasm wasmhttp.Wasm
	if err := retried.TypedConfig.UnmarshalTo(&wasm); err != nil {
		return nil, fmt.Errorf("decode rejected WASM configuration: %w", err)
	}
	vm := wasm.GetConfig().GetVmConfig()
	if vm == nil {
		return nil, errors.New("rejected WASM configuration has no VM")
	}
	// Envoy remembers the typed-config hash even after a NACK, so a version-only
	// retry is skipped and the filter is never created. Changing the VM ID makes
	// the typed config differ and Envoy instantiates it from the fetched module.
	// shortcut: the VM ID doubles as a typed-config generation marker; drop this
	// once Envoy retries failed filter creations on its own.
	vm.VmId = fmt.Sprintf("%s.retry%d", vm.VmId, attempt)
	typed, err := anypb.New(&wasm)
	if err != nil {
		return nil, fmt.Errorf("encode retry WASM configuration: %w", err)
	}
	retried.TypedConfig = typed
	compiled, err := cachev3.NewSnapshot(fmt.Sprintf("%s.retry%d", meta.Version, attempt), map[resource.Type][]cachetypes.Resource{
		resource.ExtensionConfigType: {retried},
	})
	if err != nil {
		return nil, fmt.Errorf("build retry snapshot: %w", err)
	}
	return compiled, nil
}

// BindRejectedRetry attaches a stream to its retry snapshot. A reconnect after
// an accepted retry can recreate the derived resource from its version_info.
func (s *SnapshotStore) BindRejectedRetry(resourceName, proxyKey, version string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key, meta, ok := s.snapshotForResourceLocked(resourceName)
	if !ok {
		return false, nil
	}
	proxy := s.proxies[proxyKey]
	if proxy != nil && proxy.engineKey != key {
		return false, fmt.Errorf("proxy cache key %q belongs to another Engine", proxyKey)
	}
	if proxy != nil {
		cached, err := s.cache.GetSnapshot(proxyKey)
		if err == nil {
			cachedVersion := cached.GetVersion(resource.ExtensionConfigType)
			if retryAttempt(cachedVersion) > 0 && retryBase(cachedVersion) == meta.Version {
				proxy.active++
				return true, nil
			}
		}
	}
	attempt := retryAttempt(version)
	if attempt == 0 || retryBase(version) != meta.Version {
		return false, nil
	}
	compiled, err := rejectedSnapshot(meta, attempt)
	if err != nil {
		return false, err
	}
	if err := s.cache.SetSnapshot(context.Background(), proxyKey, compiled); err != nil {
		return false, err
	}
	if proxy == nil {
		proxy = &proxySnapshot{engineKey: key, seq: s.nextSeq}
		s.nextSeq++
		s.trackProxyLocked(proxyKey, proxy)
	}
	proxy.active++
	return true, nil
}

// CurrentVersionFor returns a proxy's current retry version when present, or
// the Engine's accepted version otherwise.
func (s *SnapshotStore) CurrentVersionFor(resourceName, proxyKey string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key, meta, ok := s.snapshotForResourceLocked(resourceName)
	if !ok {
		return "", fmt.Errorf("no snapshot for resource %q", resourceName)
	}
	if proxy := s.proxies[proxyKey]; proxy != nil && proxy.engineKey == key {
		if cached, err := s.cache.GetSnapshot(proxyKey); err == nil {
			version := cached.GetVersion(resource.ExtensionConfigType)
			if retryAttempt(version) > 0 && retryBase(version) == meta.Version {
				return version, nil
			}
		}
	}
	cached, err := s.cache.GetSnapshot(key)
	if err != nil {
		return "", err
	}
	return cached.GetVersion(resource.ExtensionConfigType), nil
}

// AckRejected marks a proxy's derived version as accepted.
func (s *SnapshotStore) AckRejected(proxyKey, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	proxy := s.proxies[proxyKey]
	if proxy == nil || retryAttempt(version) == 0 {
		return
	}
	cached, err := s.cache.GetSnapshot(proxyKey)
	if err == nil && cached.GetVersion(resource.ExtensionConfigType) == version {
		proxy.retryUnacked = false
	}
}

// ReleaseRejectedRetry drops a stream's reference to a proxy retry snapshot.
func (s *SnapshotStore) ReleaseRejectedRetry(proxyKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	proxy := s.proxies[proxyKey]
	if proxy == nil {
		return
	}
	engineKey := proxy.engineKey
	if proxy.active > 0 {
		proxy.active--
	}
	if proxy.active == 0 {
		// A proxy that just disconnected is the most recent disconnect: its
		// snapshot should outlive older inactive retries.
		proxy.seq = s.nextSeq
		s.nextSeq++
	}
	if proxy.active == 0 && !proxy.retryUnacked {
		s.cache.ClearSnapshot(proxyKey)
		s.untrackProxyLocked(proxyKey)
	}
	// Disconnects can push an Engine over the inactive-retry bound too; a
	// burst of proxies that NACK and leave must not wait for the next NACK
	// to be reclaimed.
	s.evictInactiveRetriesLocked(engineKey)
}

func (s *SnapshotStore) snapshotForResourceLocked(resourceName string) (string, Snapshot, bool) {
	// Indexed lookup: the request path (subscriptions and ACKs) must not scan
	// every Engine under the lock.
	key, ok := s.resources[resourceName]
	if !ok {
		return "", Snapshot{}, false
	}
	snapshot, ok := s.meta[key]
	if !ok {
		return "", Snapshot{}, false
	}
	return key, snapshot, true
}

// DeleteEngine removes an Engine's snapshot and any open watches.
func (s *SnapshotStore) DeleteEngine(namespace, engineName string) {
	key := namespacedNameKey(namespace, engineName)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache.ClearSnapshot(key)
	delete(s.resources, s.meta[key].ResourceName)
	delete(s.meta, key)
	for proxyKey := range s.byEngine[key] {
		s.cache.ClearSnapshot(proxyKey)
		s.untrackProxyLocked(proxyKey)
	}
	delete(s.byEngine, key)
}

// CurrentVersion returns the Engine's shared ECDS version for a resource.
func (s *SnapshotStore) CurrentVersion(resourceName string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, _, ok := s.snapshotForResourceLocked(resourceName)
	if !ok {
		return "", fmt.Errorf("no snapshot for resource %q", resourceName)
	}
	cached, err := s.cache.GetSnapshot(key)
	if err != nil {
		return "", err
	}
	return cached.GetVersion(resource.ExtensionConfigType), nil
}

// Get returns the published version for an Engine key ("namespace/name").
func (s *SnapshotStore) Get(engineKey string) (*Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta, ok := s.meta[engineKey]
	if !ok {
		return nil, false
	}
	if _, err := s.cache.GetSnapshot(engineKey); err != nil {
		return nil, false
	}
	out := meta
	return &out, true
}

// namespacedNameKey is the "<namespace>/<name>" key shared by snapshot
// stores, authorization identities, and readiness records. It matches
// types.NamespacedName.String(), which the publication controller relies on
// when it resolves a reconcile request to a snapshot key.
func namespacedNameKey(namespace, name string) string {
	return types.NamespacedName{Namespace: namespace, Name: name}.String()
}

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
