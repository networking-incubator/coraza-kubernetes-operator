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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"weak"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasmhttp "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	cachetypes "github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
	"k8s.io/apimachinery/pkg/types"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

// tag tells one delivery of an Engine's configuration from another. Every
// resource the store serves carries one in its xDS version and in its WASM VM
// ID, so a proxy that rejected a configuration is never sent the same bytes
// again, which Envoy would accept without building them. The epoch is random
// per process and the generation advances within it, so no two processes send
// the same tag. The accepted-configuration records and the readiness checks
// compare content versions, which never carry a tag.
type tag struct {
	epoch string
	gen   uint64
}

func (t tag) String() string { return t.epoch + "-" + strconv.FormatUint(t.gen, 10) }

// wireVersion is the xDS version of an Engine's content sent under a tag.
func wireVersion(version string, t tag) string { return version + "." + t.String() }

// tagNode is the cache node of one tag snapshot of an Engine.
func tagNode(engineKey string, t tag) string { return engineKey + "\x00tag:" + t.String() }

// epochBytes is the size of the random epoch, which a tag writes in hexadecimal.
const epochBytes = 8

// parseWireVersion splits a version a proxy reports into the content version
// and the tag it carries. The version is a client string, so it is accepted
// only in the form wireVersion writes, with no other digits or letters in the
// tag: bare content hashes and the retry versions of earlier releases are not.
func parseWireVersion(wire string) (version string, t tag, ok bool) {
	version, rest, found := strings.CutLast(wire, ".")
	if !found || version == "" {
		return "", tag{}, false
	}
	epoch, gen, found := strings.Cut(rest, "-")
	if !found || len(epoch) != 2*epochBytes {
		return "", tag{}, false
	}
	if raw, err := hex.DecodeString(epoch); err != nil || hex.EncodeToString(raw) != epoch {
		return "", tag{}, false
	}
	n, err := strconv.ParseUint(gen, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != gen {
		return "", tag{}, false
	}
	return version, tag{epoch: epoch, gen: n}, true
}

// content is an Engine's rendered configuration, decoded once. The resource of
// every tag is rendered from it when a response is marshaled, so a tag holds no
// bytes of its own: the plugin configuration is megabytes of rules, and every tag
// shares the one copy.
type content struct {
	version string
	name    string
	// template is the WASM configuration without its two configuration fields.
	template *wasmhttp.Wasm
	// pluginConfiguration and vmConfiguration are those fields. They are shared
	// by every rendering and never modified.
	pluginConfiguration *anypb.Any
	vmConfiguration     *anypb.Any

	// log tells a rendering that failed. What decoded encodes, so none should:
	// it is logged once for the content, however many tags and responses it spoils.
	log    logr.Logger
	failed atomic.Bool
}

// newContent decodes the snapshot's resource. It fails for one that cannot carry
// a tag.
func newContent(snapshot *Snapshot, log logr.Logger) (*content, error) {
	var wasm wasmhttp.Wasm
	if err := snapshot.Resource.GetTypedConfig().UnmarshalTo(&wasm); err != nil {
		return nil, fmt.Errorf("decode WASM configuration: %w", err)
	}
	plugin := wasm.GetConfig()
	vm := plugin.GetVmConfig()
	if vm == nil {
		return nil, errors.New("WASM configuration has no VM")
	}
	c := &content{
		version:             snapshot.Version,
		name:                snapshot.Resource.GetName(),
		template:            &wasm,
		pluginConfiguration: plugin.Configuration,
		vmConfiguration:     vm.Configuration,
		log:                 log,
	}
	plugin.Configuration, vm.Configuration = nil, nil
	return c, nil
}

// render returns the content's resource under a tag: the same resource with the
// tag in its VM ID, sharing the configuration with every other rendering. A
// failed rendering is a resource without its configuration, which a proxy
// rejects instead of applying part of one.
func (c *content) render(t tag) *corev3.TypedExtensionConfig {
	wasm := proto.Clone(c.template).(*wasmhttp.Wasm)
	plugin := wasm.GetConfig()
	plugin.Configuration = c.pluginConfiguration
	vm := plugin.GetVmConfig()
	vm.Configuration = c.vmConfiguration
	vm.VmId += "." + t.String()
	typed := &anypb.Any{}
	if err := anypb.MarshalFrom(typed, wasm, proto.MarshalOptions{Deterministic: true}); err != nil {
		if c.failed.CompareAndSwap(false, true) {
			c.log.Error(err, "Failed to render ECDS resource", "resource", c.name, "version", c.version, "tag", t.String())
		}
		return &corev3.TypedExtensionConfig{Name: c.name}
	}
	return &corev3.TypedExtensionConfig{Name: c.name, TypedConfig: typed}
}

// tagResource is an Engine's resource under one tag, as the cache holds it: a few
// words, rendered when a response is marshaled. go-control-plane keeps what it
// marshals for the response only, so no snapshot holds the bytes.
type tagResource struct {
	content *content
	tag     tag

	mu sync.Mutex
	// rendered is the rendering the responses in flight marshal. It is weak:
	// responses that overlap, as when many proxies are sent the same tag at once,
	// share one rendering, and none is kept once they are done.
	rendered weak.Pointer[corev3.TypedExtensionConfig]
}

// GetName is the resource's ECDS name, which the cache indexes it by.
func (r *tagResource) GetName() string { return r.content.name }

// ProtoReflect renders the resource, unless a response in flight has rendered it.
func (r *tagResource) ProtoReflect() protoreflect.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	rendered := r.rendered.Value()
	if rendered == nil {
		rendered = r.content.render(r.tag)
		r.rendered = weak.Make(rendered)
	}
	return rendered.ProtoReflect()
}

// SnapshotStore publishes each Engine's configuration into go-control-plane's
// SnapshotCache as tag snapshots: one per tag a stream holds, shared by the
// streams that hold it. A tag snapshot is a few words, so the tags in use cost
// no more than the streams, however many distinct tags the proxies report.
type SnapshotStore struct {
	// Logger tells what the store could not do, which should not happen.
	Logger logr.Logger

	cache cachev3.SnapshotCache
	epoch string

	mu      sync.RWMutex
	engines map[string]*engineState
	// onChange is called, outside the lock, once Replace has published a
	// version the Engine did not have, so that streams waiting out a rejection
	// of the old configuration can pick the new one up at once.
	onChange func(engineKey string)
}

// engineState is one Engine's published content and the tags streams hold.
type engineState struct {
	// meta is the published snapshot, without its resource, which content
	// replaces. content is nil until the Engine is published.
	meta    Snapshot
	content *content
	// gen is the generation of the Engine's current tag.
	gen uint64
	// renders counts the times content was published.
	renders uint64
	// tags maps what streams hold to the number of leases on it. The cache holds
	// a snapshot of each at its node, once the Engine is published.
	tags map[heldTag]int
}

func (e *engineState) published() bool { return e.content != nil }

// heldTag is what a stream holds: a tag and, for a tag another process minted,
// the publication it was held in. Publishing sends the holders of such a tag this
// process's own, so a proxy that reports the tag afterwards must not share their
// snapshot: it is replayed the bytes it reported from one of its own.
type heldTag struct {
	tag    tag
	render uint64
}

// node is the cache node of the snapshot held.
func (h heldTag) node(engineKey string) string {
	if h.render == 0 {
		return tagNode(engineKey, h.tag)
	}
	return tagNode(engineKey, h.tag) + "\x00" + strconv.FormatUint(h.render, 10)
}

// tagLease is a stream's hold on one tag snapshot.
type tagLease struct {
	engineKey string
	// eng is the state the lease was taken from: a deleted Engine's lease must
	// not release a snapshot of its successor, which is another state.
	eng *engineState
	heldTag
	// claimed is whether the version the proxy reported chose the tag, whose
	// snapshot carries exactly the bytes it reported.
	claimed bool
}

func (l tagLease) node() string { return l.heldTag.node(l.engineKey) }

// NewSnapshotStore returns an empty snapshot store.
func NewSnapshotStore() *SnapshotStore {
	var epoch [epochBytes]byte
	_, _ = rand.Read(epoch[:])
	return &SnapshotStore{
		Logger:  logr.Discard(),
		cache:   cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil),
		epoch:   hex.EncodeToString(epoch[:]),
		engines: make(map[string]*engineState),
	}
}

// setOnChange registers the function Replace calls for a new version.
func (s *SnapshotStore) setOnChange(fn func(engineKey string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = fn
}

func (s *SnapshotStore) currentLocked(eng *engineState) tag {
	return tag{epoch: s.epoch, gen: eng.gen}
}

// engineLocked returns the Engine's state, creating it unpublished.
func (s *SnapshotStore) engineLocked(engineKey string) *engineState {
	eng := s.engines[engineKey]
	if eng == nil {
		eng = &engineState{tags: make(map[heldTag]int)}
		s.engines[engineKey] = eng
	}
	return eng
}

// tagSnapshot is the cache snapshot of content sent under a tag.
func tagSnapshot(c *content, sent tag) (cachev3.ResourceSnapshot, error) {
	snapshot, err := cachev3.NewSnapshot(wireVersion(c.version, sent), map[resource.Type][]cachetypes.Resource{
		resource.ExtensionConfigType: {&tagResource{content: c, tag: sent}},
	})
	if err != nil {
		return nil, fmt.Errorf("build snapshot: %w", err)
	}
	return snapshot, nil
}

// Replace publishes an Engine's configuration under every tag a stream holds.
// The cache key of a tag snapshot is derived from the Engine key, not the ECDS
// resource name, so it survives a UID change and can be deleted by
// namespace/name. A Version that is already published is the same content and
// is not decoded again.
func (s *SnapshotStore) Replace(snapshot *Snapshot) error {
	if snapshot == nil || snapshot.Resource == nil {
		return errors.New("snapshot and resource are required")
	}
	key := namespacedNameKey(snapshot.Namespace, snapshot.EngineName)
	meta := *snapshot
	meta.Resource = nil
	if s.refreshUnchanged(key, meta) {
		return nil
	}
	c, err := newContent(snapshot, s.Logger)
	if err != nil {
		return err
	}
	notify, err := s.publish(key, meta, c)
	if notify != nil {
		notify(key)
	}
	return err
}

// refreshUnchanged records a snapshot of the content already published, as when
// only the inputs a proxy does not see changed, and reports whether it did.
func (s *SnapshotStore) refreshUnchanged(key string, meta Snapshot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	eng := s.engines[key]
	if eng == nil || !eng.published() || eng.meta.Version != meta.Version {
		return false
	}
	eng.meta = meta
	return true
}

// publish installs the content at every tag a stream holds, and only then
// records it as the Engine's, so that a publication that failed is tried again.
// A tag another process minted is not sent new content: that process may send the
// proxy the same tag with the same bytes, which Envoy would accept without building
// them if the proxy rejected them here. The proxies that hold it are sent this
// process's current tag instead, from the snapshot of the streams that hold that
// tag. It returns the function to call, once the lock is released, when the Engine
// has a version it did not have.
func (s *SnapshotStore) publish(key string, meta Snapshot, c *content) (func(string), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	eng := s.engineLocked(key)
	eng.renders++
	current := s.currentLocked(eng)
	built := make(map[tag]cachev3.ResourceSnapshot)
	for h := range eng.tags {
		sent := h.tag
		if h.tag.epoch != s.epoch {
			sent = current
		}
		snapshot, ok := built[sent]
		if !ok {
			var err error
			if snapshot, err = tagSnapshot(c, sent); err != nil {
				return nil, err
			}
			built[sent] = snapshot
		}
		if err := s.cache.SetSnapshot(context.Background(), h.node(key), snapshot); err != nil {
			return nil, err
		}
	}
	var notify func(string)
	if !eng.published() || eng.meta.Version != meta.Version {
		notify = s.onChange
	}
	eng.meta, eng.content = meta, c
	return notify, nil
}

// Acquire holds the tag snapshot an Engine's stream is served from, and returns
// the lease to give back. A proxy reports the version it last accepted: when
// that is a tag of the Engine's current content, the first request of its stream
// is answered with exactly those bytes again, whichever process sent them, a full
// response that Envoy skips building, so the proxy keeps its filter when it moves
// to another replica or the operator restarts. Otherwise it is served the
// Engine's current tag. The report is a client string, but a tag costs a few
// words and lasts as long as the stream that holds it, so every report of the
// current content is honoured, except a generation this process has not reached.
func (s *SnapshotStore) Acquire(engineKey, claim string) (tagLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	eng := s.engineLocked(engineKey)
	held, claimed := s.wantLocked(eng, claim)
	if _, ok := eng.tags[held]; !ok && eng.published() {
		snapshot, err := tagSnapshot(eng.content, held.tag)
		if err == nil {
			err = s.cache.SetSnapshot(context.Background(), held.node(engineKey), snapshot)
		}
		if err != nil {
			return tagLease{}, err
		}
	}
	eng.tags[held]++
	return tagLease{engineKey: engineKey, eng: eng, heldTag: held, claimed: claimed}, nil
}

// wantLocked is what a stream is served: the tag the proxy reported when it is a
// tag of the Engine's current content that this process could have sent, held for
// the current publication if another process minted it, the Engine's current tag
// otherwise.
func (s *SnapshotStore) wantLocked(eng *engineState, claim string) (held heldTag, claimed bool) {
	version, reported, ok := parseWireVersion(claim)
	if ok && eng.published() && version == eng.meta.Version && (reported.epoch != s.epoch || reported.gen <= eng.gen) {
		held = heldTag{tag: reported}
		if reported.epoch != s.epoch {
			held.render = eng.renders
		}
		return held, true
	}
	return heldTag{tag: s.currentLocked(eng)}, false
}

// Release gives back a lease, and drops the tag snapshot when no stream holds it.
func (s *SnapshotStore) Release(lease tagLease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	eng := s.engines[lease.engineKey]
	held := lease.heldTag
	if eng == nil || eng != lease.eng || eng.tags[held] == 0 {
		return
	}
	eng.tags[held]--
	if eng.tags[held] > 0 {
		return
	}
	delete(eng.tags, held)
	s.cache.ClearSnapshot(lease.node())
	if !eng.published() && len(eng.tags) == 0 {
		delete(s.engines, lease.engineKey)
	}
}

// Advance moves the Engine on to the next generation when its current tag is
// from, and reports whether it did. A proxy that rejected a tag, or may have, is
// then sent a tag with other bytes; several proxies that reject the same tag
// advance the generation once.
func (s *SnapshotStore) Advance(engineKey string, from tag) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	eng := s.engines[engineKey]
	if eng == nil || from != s.currentLocked(eng) {
		return false
	}
	eng.gen++
	return true
}

// isCurrent reports whether t is the Engine's current tag.
func (s *SnapshotStore) isCurrent(engineKey string, t tag) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	eng := s.engines[engineKey]
	return eng != nil && t == s.currentLocked(eng)
}

// DeleteEngine removes an Engine's snapshots and any open watches.
func (s *SnapshotStore) DeleteEngine(namespace, engineName string) {
	key := namespacedNameKey(namespace, engineName)
	s.mu.Lock()
	defer s.mu.Unlock()
	eng := s.engines[key]
	if eng == nil {
		return
	}
	for h := range eng.tags {
		s.cache.ClearSnapshot(h.node(key))
	}
	delete(s.engines, key)
}

// Get returns the published snapshot for an Engine key ("namespace/name"),
// without its resource.
func (s *SnapshotStore) Get(engineKey string) (*Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	eng := s.engines[engineKey]
	if eng == nil || !eng.published() {
		return nil, false
	}
	out := eng.meta
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
// mutate any field after Replace hands it to the store. Version identifies the
// content: Replace reads the Resource of a Version it has not published, and
// does not keep it, so Get returns the Snapshot without one.
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
