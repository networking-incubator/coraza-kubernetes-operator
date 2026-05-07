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

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	rcache "github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
)

// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts/token,verbs=create
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create

const (
	// tokenDuration is the requested lifetime for cache client tokens.
	// Kept short (1 hour) to limit the exposure window since tokens are
	// stored in plaintext in WasmPlugin CRs. The renewal mechanism
	// requeues at 80% of the actual lifetime (~48 minutes).
	tokenDuration = 1 * time.Hour

	// tokenRenewalFraction is the fraction of a token's actual lifetime at which
	// it should be renewed. For example, 0.8 means renew after 80% of the lifetime
	// has elapsed. This avoids pathological 1-minute requeue loops when the
	// apiserver returns a shorter lifetime than requested (e.g., max token lifetime
	// configured to <=24h).
	tokenRenewalFraction = 0.8
)

// tokenExpirationSeconds is tokenDuration in seconds for the TokenRequest API.
var tokenExpirationSeconds = int64(tokenDuration.Seconds())

// -----------------------------------------------------------------------------
// TokenEntry
// -----------------------------------------------------------------------------

// TokenEntry holds a cache client token and its issuance/expiry times.
type TokenEntry struct {
	Token     string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// RenewalDeadline returns the absolute time at which the token should be renewed.
func (e *TokenEntry) RenewalDeadline() time.Time {
	lifetime := e.ExpiresAt.Sub(e.IssuedAt)
	renewAfter := time.Duration(float64(lifetime) * tokenRenewalFraction)
	return e.IssuedAt.Add(renewAfter)
}

// NeedsRenewal returns true if the token has passed the renewal point
// (i.e., more than tokenRenewalFraction of its lifetime has elapsed).
func (e *TokenEntry) NeedsRenewal() bool {
	return !time.Now().Before(e.RenewalDeadline())
}

// -----------------------------------------------------------------------------
// Engine Controller - Token Management
// -----------------------------------------------------------------------------

// cacheClientSALabels returns the labels used to identify a cache client
// ServiceAccount owned by a specific Engine.
func cacheClientSALabels(engineName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "coraza-kubernetes-operator",
		"app.kubernetes.io/component":  "cache-client",
		"app.kubernetes.io/instance":   engineName,
	}
}

// ensureCacheClientServiceAccount creates a ServiceAccount for cache client
// authentication if it doesn't already exist. The SA uses GenerateName with
// the "coraza-engine-" prefix and is discovered by label on subsequent calls.
// An owner reference is set to the Engine so the SA is garbage-collected when
// the Engine is deleted.
//
// Returns the actual ServiceAccount name so callers can use it for token creation.
func (r *EngineReconciler) ensureCacheClientServiceAccount(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) (string, error) {
	labels := cacheClientSALabels(engine.Name)

	// Discover existing SA by label.
	var saList corev1.ServiceAccountList
	if err := r.List(ctx, &saList,
		client.InNamespace(req.Namespace),
		client.MatchingLabels(labels),
	); err != nil {
		return "", fmt.Errorf("failed to list ServiceAccounts for engine %s: %w", engine.Name, err)
	}
	if len(saList.Items) > 0 {
		if len(saList.Items) > 1 {
			log.Info("Multiple ServiceAccounts match cache-client labels, using first",
				"engine", req.Name,
				"namespace", req.Namespace,
				"matchCount", len(saList.Items),
				"selectedSA", saList.Items[0].Name,
			)
		}
		logDebug(log, req, "Engine", "ServiceAccount already exists")
		return saList.Items[0].Name, nil
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: rcache.CacheEngineSAPrefix,
			Namespace:    req.Namespace,
			Labels:       labels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: wafv1alpha1.GroupVersion.String(),
					Kind:       "Engine",
					Name:       engine.Name,
					UID:        engine.UID,
					Controller: new(true),
				},
			},
		},
	}

	if err := r.Create(ctx, sa); err != nil {
		return "", fmt.Errorf("failed to create ServiceAccount for engine %s: %w", engine.Name, err)
	}

	logInfo(log, req, "Engine", "Created cache client ServiceAccount", "serviceAccount", fmt.Sprintf("%s/%s", req.Namespace, sa.Name))
	return sa.Name, nil
}

// ensureCacheToken returns a valid cache client token for the given Engine and
// the deadline at which the token should be renewed.
// If the stored token is missing or near expiry, a new one is generated via
// the Kubernetes TokenRequest API. The token audience encodes the RuleSet
// being accessed ("coraza-cache:namespace/rulesetName").
func (r *EngineReconciler) ensureCacheToken(ctx context.Context, log logr.Logger, req ctrl.Request, saName, rulesetName string) (string, time.Time, error) {
	key := fmt.Sprintf("%s/%s/%s", req.Namespace, req.Name, rulesetName)
	audience := rcache.Audience(fmt.Sprintf("%s/%s", req.Namespace, rulesetName))

	// Check if we have a valid cached token.
	if val, ok := r.tokenStore.Load(key); ok {
		entry := val.(*TokenEntry)
		if !entry.NeedsRenewal() {
			logDebug(log, req, "Engine", "Using cached token", "expiresAt", entry.ExpiresAt)
			return entry.Token, entry.RenewalDeadline(), nil
		}
	}

	logInfo(log, req, "Engine", "Generating new cache client token", "serviceAccount", saName)

	tokenReq := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &tokenExpirationSeconds,
		},
	}

	result, err := r.kubeClient.CoreV1().ServiceAccounts(req.Namespace).CreateToken(
		ctx, saName, tokenReq, metav1.CreateOptions{},
	)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create token for SA %s/%s: %w", req.Namespace, saName, err)
	}

	if result.Status.Token == "" {
		return "", time.Time{}, fmt.Errorf("TokenRequest returned empty token for SA %s/%s", req.Namespace, saName)
	}

	now := time.Now()
	expiresAt := result.Status.ExpirationTimestamp.Time
	entry := &TokenEntry{Token: result.Status.Token, IssuedAt: now, ExpiresAt: expiresAt}
	r.tokenStore.Store(key, entry)

	// Lazily prune expired entries to prevent unbounded growth when
	// Engines are deleted and never reconciled again.
	r.pruneExpiredTokens()

	logInfo(log, req, "Engine", "Generated cache client token", "expiresAt", expiresAt)
	return result.Status.Token, entry.RenewalDeadline(), nil
}

// pruneExpiredTokens removes all expired entries from the token store.
func (r *EngineReconciler) pruneExpiredTokens() {
	now := time.Now()
	r.tokenStore.Range(func(key, value any) bool {
		entry := value.(*TokenEntry)
		if now.After(entry.ExpiresAt) {
			r.tokenStore.Delete(key)
		}
		return true
	})
}

// cleanupStaleTokens removes token entries for RuleSets that an Engine no
// longer references. When spec.ruleSet.name changes, the old token (keyed by
// "namespace/engineName/oldRuleSet") would otherwise leak in the sync.Map
// until it expires.
func (r *EngineReconciler) cleanupStaleTokens(namespace, engineName, currentRuleSet string) {
	prefix := fmt.Sprintf("%s/%s/", namespace, engineName)
	r.tokenStore.Range(func(key, _ any) bool {
		k := key.(string)
		if strings.HasPrefix(k, prefix) && !strings.HasSuffix(k, "/"+currentRuleSet) {
			r.tokenStore.Delete(k)
		}
		return true
	})
}
