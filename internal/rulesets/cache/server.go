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

package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	authclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
)

// -----------------------------------------------------------------------------
// Constants
// -----------------------------------------------------------------------------

const (
	// TimestampFormat is the RFC3339 format with milliseconds used for all timestamps
	TimestampFormat = time.RFC3339Nano

	// CacheGCInterval is how often to check for and remove stale cache entries
	CacheGCInterval = 5 * time.Minute

	// CacheMaxAge is the maximum age of a cache entry before it's considered stale
	CacheMaxAge = 24 * time.Hour

	// CacheMaxSize is the maximum total size of all cached rules in bytes (100MB)
	CacheMaxSize = 100 * 1024 * 1024

	// MaxHeaderSize is the maximum size of HTTP request headers (64KB)
	MaxHeaderSize = 64 * 1024

	// MaxBodySize is the maximum size of HTTP request bodies (0 bytes - no body expected)
	MaxBodySize = 0

	// ReadTimeout is the maximum duration for reading the entire request
	ReadTimeout = 15 * time.Second

	// WriteTimeout is the maximum duration before timing out writes of the response
	WriteTimeout = 15 * time.Second

	// IdleTimeout is the maximum time to wait for the next request when keep-alives are enabled
	IdleTimeout = 60 * time.Second

	// GracefulShutdownTimeout is the max time to drain existing connections on shutdown
	GracefulShutdownTimeout = 10 * time.Second
)

// -----------------------------------------------------------------------------
// API Response Types
// -----------------------------------------------------------------------------

// LatestResponse contains metadata about the latest ruleset version
type LatestResponse struct {
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
}

// -----------------------------------------------------------------------------
// RuleSetCacheServer
// -----------------------------------------------------------------------------

// ruleSetCacheServer provides HTTP endpoints for accessing cached rulesets
type ruleSetCacheServer struct {
	cache  *RuleSetCache
	auth   *TokenAuthenticator
	srv    *http.Server
	logger logr.Logger
	gc     GarbageCollectionConfig
}

// NewServer creates a new RuleSetCacheServer instance.
// The tokenReview client is used to validate ServiceAccount JWT tokens on incoming requests.
func NewServer(cache *RuleSetCache, addr string, logger logr.Logger, gc *GarbageCollectionConfig, tokenReview authclient.TokenReviewInterface) *ruleSetCacheServer {
	gcConfig := DefaultGC()
	if gc != nil {
		gcConfig = *gc
	}

	s := &ruleSetCacheServer{
		cache:  cache,
		auth:   NewTokenAuthenticator(tokenReview),
		logger: logger,
		gc:     gcConfig,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/rules/", s.handleRules)

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           instrumentHandler(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderSize,
	}

	return s
}

// Start the cache server and the GC loop. Both are stopped when ctx is cancelled.
func (s *ruleSetCacheServer) Start(ctx context.Context) error {
	go s.rungc(ctx)

	errChan := make(chan error, 1)
	go func() {
		s.logger.Info("Starting ruleset cache server", "addr", s.srv.Addr)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("Shutting down ruleset cache server")
		s.srv.SetKeepAlivesEnabled(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), GracefulShutdownTimeout)
		defer cancel()

		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.logger.Error(err, "Error during graceful shutdown, forcing close")
			return s.srv.Close()
		}

		s.logger.Info("Cache server shutdown complete")
		return nil
	case err := <-errChan:
		return err
	}
}

// NeedLeaderElection implements the LeaderElectionRunnable interface.
func (s *ruleSetCacheServer) NeedLeaderElection() bool {
	return false
}

// -----------------------------------------------------------------------------
// RuleSetCacheServer - Handlers
// -----------------------------------------------------------------------------

func (s *ruleSetCacheServer) handleRules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodySize)

	path := strings.TrimPrefix(r.URL.Path, "/rules/")
	if path == "" {
		http.Error(w, "RuleSet key required", http.StatusBadRequest)
		return
	}

	// Determine the cache key (strip /latest suffix if present).
	cacheKey := path
	isLatest := false
	if stripped, ok := strings.CutSuffix(path, "/latest"); ok {
		cacheKey = stripped
		isLatest = true
	}

	// Authenticate: the token audience must match the requested RuleSet, and
	// the SA namespace must match the cache key namespace.
	if err := s.authenticateRequest(r, cacheKey); err != nil {
		s.logger.Info("Authentication failed", "cacheKey", cacheKey, "error", err)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if isLatest {
		s.handleLatest(w, r, cacheKey)
		return
	}

	s.handleGetRules(w, r, cacheKey)
}

// authenticateRequest validates the Bearer token from the request and checks
// that the ServiceAccount namespace matches the cache key namespace.
// The audience-scoped TokenReview ensures the token is authorized for the
// specific RuleSet being accessed (audience = "coraza-cache:namespace/rulesetName").
func (s *ruleSetCacheServer) authenticateRequest(r *http.Request, cacheKey string) error {
	token := extractBearerToken(r)
	if token == "" {
		return fmt.Errorf("missing bearer token")
	}

	audience := Audience(cacheKey)
	result, err := s.auth.Authenticate(r.Context(), token, audience)
	if err != nil {
		return err
	}

	// Extract the namespace from the cache key and verify the SA lives in it.
	keyNS, _, ok := strings.Cut(cacheKey, "/")
	if !ok {
		return fmt.Errorf("invalid cache key format: %s", cacheKey)
	}
	if result.Namespace != keyNS {
		return fmt.Errorf("service account namespace %s does not match cache key namespace %s", result.Namespace, keyNS)
	}

	return nil
}

// extractBearerToken extracts the token from the Authorization header.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return auth[len(prefix):]
	}
	return ""
}

func (s *ruleSetCacheServer) handleLatest(w http.ResponseWriter, _ *http.Request, cacheKey string) {
	entry, ok := s.cache.Get(cacheKey)
	if !ok {
		http.Error(w, "RuleSet not found", http.StatusNotFound)
		return
	}

	response := LatestResponse{
		UUID:      entry.UUID,
		Timestamp: entry.Timestamp.Format(TimestampFormat),
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(response); err != nil {
		s.logger.Error(err, "Failed to encode latest response")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func (s *ruleSetCacheServer) handleGetRules(w http.ResponseWriter, _ *http.Request, cacheKey string) {
	entry, ok := s.cache.Get(cacheKey)
	if !ok {
		http.Error(w, "RuleSet not found", http.StatusNotFound)
		return
	}

	s.logger.Info("Serving rules from cache", "cacheKey", cacheKey, "uuid", entry.UUID, "availableKeysCount", s.cache.Len(), "cacheSizeBytes", s.cache.TotalSize())

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(entry); err != nil {
		s.logger.Error(err, "Failed to encode rules response")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// -----------------------------------------------------------------------------
// RuleSetCacheServer - Garbage Collection
// -----------------------------------------------------------------------------

// GarbageCollectionConfig is the GC config for the RuleSetCacheServer.
type GarbageCollectionConfig struct {
	// GCInterval is how often to check for and remove stale cache entries.
	GCInterval time.Duration

	// MaxAge is the maximum age of a cache entry before it's considered stale.
	MaxAge time.Duration

	// MaxSize is the maximum total size of all cached rules in bytes.
	MaxSize int
}

// DefaultGC returns the default garbage collection configuration.
func DefaultGC() GarbageCollectionConfig {
	return GarbageCollectionConfig{
		GCInterval: CacheGCInterval,
		MaxAge:     CacheMaxAge,
		MaxSize:    CacheMaxSize,
	}
}

// rungc periodically removes stale cache entries using two strategies:
// 1. Age-based: entries older than MaxAge (except latest)
// 2. Size-based: oldest entries when cache exceeds MaxSize (except latest)
func (s *ruleSetCacheServer) rungc(ctx context.Context) {
	ticker := time.NewTicker(s.gc.GCInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prunedByAge := s.cache.Prune(s.gc.MaxAge)
			if prunedByAge > 0 {
				gcPrunedEntriesTotal.WithLabelValues(PruneReasonAge).Add(float64(prunedByAge))
				s.logger.Info("Pruned stale cache entries by age", "count", prunedByAge, "maxAge", s.gc.MaxAge)
			}

			currentSize := s.cache.TotalSize()
			if currentSize > s.gc.MaxSize {
				prunedBySize := s.cache.PruneBySize(s.gc.MaxSize)
				if prunedBySize > 0 {
					gcPrunedEntriesTotal.WithLabelValues(PruneReasonSize).Add(float64(prunedBySize))
					s.logger.Info("Pruned cache entries by size", "count", prunedBySize, "maxSize", s.gc.MaxSize, "currentSize", s.cache.TotalSize())
				}

				finalSize := s.cache.TotalSize()
				if finalSize > s.gc.MaxSize {
					gcSizeLimitExceededTotal.Inc()
					s.logger.Error(errors.New("cache size exceeds maximum"), "CRITICAL: Cache size exceeds maximum even after pruning - latest entry is too large", "currentSize", finalSize, "maxSize", s.gc.MaxSize, "overage", finalSize-s.gc.MaxSize)
				}
			}
		}
	}
}
