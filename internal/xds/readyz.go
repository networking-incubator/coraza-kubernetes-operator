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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

// VerifyBundledManifest checks that the bundled module bytes match the
// manifest digest.
func VerifyBundledManifest(manifest *assets.Manifest, data []byte) error {
	if manifest == nil {
		return errors.New("bundled asset manifest is missing")
	}
	sum := sha256.Sum256(data)
	if digest := hex.EncodeToString(sum[:]); digest != manifest.SHA256 {
		return fmt.Errorf("bundled module digest mismatch: manifest %s, file %s", manifest.SHA256, digest)
	}
	return nil
}

// defaultPublicationDeadline bounds how long an unreconstructable accepted
// record can gate readiness. Without it a tenant can leave one accepted record
// this build can never reproduce (for example, its RuleSet was deleted) and
// keep every replica unready, which fails every Gateway closed.
const defaultPublicationDeadline = 2 * time.Minute

// PublicationReadiness tracks whether this replica has reconstructed every
// accepted Engine configuration present at startup. Failures after the initial
// sync do not flip the replica unready.
type PublicationReadiness struct {
	// Logger reports accepted records this build cannot publish by design.
	Logger logr.Logger
	// Metrics records accepted records that never reconstructed.
	Metrics *Metrics

	mu            sync.Mutex
	initialized   bool
	initializedAt time.Time
	deadline      time.Duration
	now           func() time.Time
	pending       map[string]string
	// outcomes holds reconciles that completed before the startup Engine list
	// was reconciled, so a fast reconcile is not lost when Initialize runs.
	outcomes map[string]string
	// expired is set once the deadline passed and the stuck records were
	// reported. They stop gating readiness from then on.
	expired bool
}

// NewPublicationReadiness returns an uninitialized tracker.
func NewPublicationReadiness() *PublicationReadiness {
	return &PublicationReadiness{
		Logger:   logr.Discard(),
		deadline: defaultPublicationDeadline,
		now:      time.Now,
		pending:  make(map[string]string),
		outcomes: make(map[string]string),
	}
}

// Initialize records the Engines present at startup that carry an accepted
// configuration this build can publish. Reconcilers may run before this list is
// available, so their outcomes are applied here rather than discarded. Later
// calls are ignored.
func (p *PublicationReadiness) Initialize(engines []*wafv1alpha1.Engine) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initialized {
		return
	}
	p.initialized = true
	p.initializedAt = p.currentTime()
	stale := 0
	for _, engine := range engines {
		if engine == nil || engine.Status == nil || engine.Status.AcceptedConfiguration == nil {
			continue
		}
		// A rendering schema bump leaves accepted records behind that this build
		// cannot reproduce by design. They must not gate readiness: an Engine
		// that never re-accepts would keep every replica unready.
		if engine.Status.AcceptedConfiguration.RenderingSchemaVersion != render.RenderingSchemaVersion {
			stale++
			continue
		}
		key := namespacedNameKey(engine.Namespace, engine.Name)
		if outcome, ok := p.outcomes[key]; ok {
			if outcome != "" {
				p.pending[key] = outcome
			}
			continue
		}
		p.pending[key] = "not reconstructed yet"
	}
	p.outcomes = nil
	if stale > 0 {
		p.Logger.Info("ignoring accepted configurations from a previous rendering schema", "count", stale)
	}
}

// Initialized reports whether the startup Engine list was processed.
func (p *PublicationReadiness) Initialized() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.initialized
}

// RecordSuccess marks an Engine as reconstructed.
func (p *PublicationReadiness) RecordSuccess(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := namespacedNameKey(namespace, name)
	delete(p.pending, key)
	if !p.initialized {
		p.outcomes[key] = ""
	}
}

// RecordFailure records a reconstruction failure and reports whether it counts
// as a cold-start failure. Failures after the initial sync for Engines that
// were already reconstructed do not flip the replica unready.
func (p *PublicationReadiness) RecordFailure(namespace, name string, err error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := namespacedNameKey(namespace, name)
	reason := "unknown failure"
	if err != nil {
		reason = err.Error()
	}

	if !p.initialized {
		p.outcomes[key] = reason
		return true
	}
	if _, pending := p.pending[key]; !pending {
		return false
	}
	p.pending[key] = reason
	return true
}

// Check reports why this replica is not ready to serve accepted state.
func (p *PublicationReadiness) Check() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized {
		return errors.New("publication initial sync is not complete")
	}
	if len(p.pending) == 0 {
		return nil
	}
	if p.deadlinePassed() {
		p.reportExpired()
		return nil
	}

	keys := make([]string, 0, len(p.pending))
	for key := range p.pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	const maxReported = 5
	reported := keys
	if len(reported) > maxReported {
		reported = reported[:maxReported]
	}
	var builder strings.Builder
	for i, key := range reported {
		if i > 0 {
			builder.WriteString("; ")
		}
		builder.WriteString(key)
		builder.WriteString(": ")
		builder.WriteString(p.pending[key])
	}
	if len(keys) > len(reported) {
		fmt.Fprintf(&builder, "; and %d more", len(keys)-len(reported))
	}
	return fmt.Errorf("accepted configurations are not reconstructed yet: %s", builder.String())
}

func (p *PublicationReadiness) currentTime() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}

func (p *PublicationReadiness) deadlinePassed() bool {
	deadline := p.deadline
	if deadline <= 0 {
		deadline = defaultPublicationDeadline
	}
	return !p.currentTime().Before(p.initializedAt.Add(deadline))
}

// reportExpired stops the remaining pending records from gating readiness and
// reports each one once. This is the last resort for an accepted record this
// build can never reconstruct. The map is cleared so a later failure of the
// same Engine is no longer counted as a cold reconstruction.
func (p *PublicationReadiness) reportExpired() {
	if p.expired {
		return
	}
	p.expired = true

	keys := make([]string, 0, len(p.pending))
	for key := range p.pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p.Logger.Info("accepted configuration was not reconstructed within the readiness deadline",
			"engine", key, "reason", p.pending[key])
		if p.Metrics != nil {
			p.Metrics.ColdReconstructionFailure()
		}
	}
	p.pending = make(map[string]string)
}

// PublicationStartup waits for the informer cache, records the startup Engine
// list in the readiness tracker, and then blocks until shutdown.
type PublicationStartup struct {
	Cache     cache.Cache
	Reader    client.Reader
	Readiness *PublicationReadiness
}

// Start implements manager.Runnable.
func (p *PublicationStartup) Start(ctx context.Context) error {
	if !p.Cache.WaitForCacheSync(ctx) {
		return nil
	}

	var list wafv1alpha1.EngineList
	if err := p.Reader.List(ctx, &list); err != nil {
		return fmt.Errorf("list Engines for publication readiness: %w", err)
	}

	engines := make([]*wafv1alpha1.Engine, 0, len(list.Items))
	for i := range list.Items {
		engines = append(engines, &list.Items[i])
	}
	p.Readiness.Initialize(engines)

	<-ctx.Done()
	return nil
}

// NeedLeaderElection runs on every replica.
func (p *PublicationStartup) NeedLeaderElection() bool {
	return false
}
