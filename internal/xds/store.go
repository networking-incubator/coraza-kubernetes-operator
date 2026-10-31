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

// SnapshotStore publishes one ECDS snapshot per Engine into go-control-plane's
// SnapshotCache. The cache key is "<namespace>/<engine>", which the stream
// callbacks write into node.id after authorization.
type SnapshotStore struct {
	cache cachev3.SnapshotCache

	mu   sync.RWMutex
	meta map[string]Snapshot
}

// NewSnapshotStore returns an empty snapshot store.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{
		cache: cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil),
		meta:  make(map[string]Snapshot),
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
	s.meta[key] = *snapshot
	return nil
}

// BumpRejected republishes the snapshot for one ECDS resource under a derived
// version and VM ID so Envoy re-applies a rejected typed configuration. The
// accepted resource and version recorded for the Engine are left untouched.
// acceptedVersion must still be the current snapshot version; a concurrent
// Replace with newer content makes the retry stale and it is skipped.
func (s *SnapshotStore) BumpRejected(resourceName, acceptedVersion string, attempt int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var key string
	var meta Snapshot
	for candidate, snapshot := range s.meta {
		if snapshot.ResourceName == resourceName {
			key, meta = candidate, snapshot
			break
		}
	}
	if key == "" {
		return fmt.Errorf("no snapshot for resource %q", resourceName)
	}
	// An empty version cannot identify the rejected snapshot (for example a
	// first-message NACK); retrying the current snapshot is safe because it
	// cannot restore older content.
	if acceptedVersion != "" && meta.Version != acceptedVersion {
		return fmt.Errorf("snapshot for resource %q advanced from %q to %q", resourceName, acceptedVersion, meta.Version)
	}

	retried := proto.Clone(meta.Resource).(*corev3.TypedExtensionConfig)
	var wasm wasmhttp.Wasm
	if err := retried.TypedConfig.UnmarshalTo(&wasm); err != nil {
		return fmt.Errorf("decode rejected WASM configuration: %w", err)
	}
	vm := wasm.GetConfig().GetVmConfig()
	if vm == nil {
		return errors.New("rejected WASM configuration has no VM")
	}
	// Envoy remembers the typed-config hash even after a NACK, so a version-only
	// retry is skipped and the filter is never created. Changing the VM ID makes
	// the typed config differ and Envoy instantiates it from the fetched module.
	// shortcut: the VM ID doubles as a typed-config generation marker; drop this
	// once Envoy retries failed filter creations on its own.
	vm.VmId = fmt.Sprintf("%s.retry%d", vm.VmId, attempt)
	typed, err := anypb.New(&wasm)
	if err != nil {
		return fmt.Errorf("encode retry WASM configuration: %w", err)
	}
	retried.TypedConfig = typed
	compiled, err := cachev3.NewSnapshot(fmt.Sprintf("%s.retry%d", meta.Version, attempt), map[resource.Type][]cachetypes.Resource{
		resource.ExtensionConfigType: {retried},
	})
	if err != nil {
		return fmt.Errorf("build retry snapshot: %w", err)
	}
	return s.cache.SetSnapshot(context.Background(), key, compiled)
}

// DeleteEngine removes an Engine's snapshot and any open watches.
func (s *SnapshotStore) DeleteEngine(namespace, engineName string) {
	key := namespacedNameKey(namespace, engineName)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache.ClearSnapshot(key)
	delete(s.meta, key)
}

// CurrentVersion returns the live cache version for an ECDS resource. It is the
// version a proxy is rejecting when a NACK arrives on a fresh stream, before
// this server has sent a response whose nonce it can match.
func (s *SnapshotStore) CurrentVersion(resourceName string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for key, meta := range s.meta {
		if meta.ResourceName != resourceName {
			continue
		}
		cached, err := s.cache.GetSnapshot(key)
		if err != nil {
			return "", err
		}
		return cached.GetVersion(resource.ExtensionConfigType), nil
	}
	return "", fmt.Errorf("no snapshot for resource %q", resourceName)
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
