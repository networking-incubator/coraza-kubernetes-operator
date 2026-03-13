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
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

const testServerAddr = ":38080"

var (
	dataFile = map[string][]byte{
		"somefile.data": []byte("some file\nanother file"),
	}
)

func TestNewServer(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, testServerAddr, logger, nil)
	require.NotNil(t, server)
	assert.Equal(t, testServerAddr, server.srv.Addr)
	assert.Equal(t, MaxHeaderSize, server.srv.MaxHeaderBytes)
	assert.False(t, server.NeedLeaderElection())
}

func TestServer_StartAndStop(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, testServerAddr, logger, nil)

	t.Log("Starting server in background goroutine")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errChan := make(chan error, 1)
	go func() {
		errChan <- server.Start(ctx)
	}()
	time.Sleep(100 * time.Millisecond)

	t.Log("Cancelling context to stop server")
	cancel()

	t.Log("Waiting for server to shut down")
	select {
	case err := <-errChan:
		if err != nil && err != http.ErrServerClosed && err.Error() != "context canceled" {
			t.Errorf("Unexpected error from server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Server did not shut down in time")
	}
}

func TestServer_HandleGetRules_Success(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, testServerAddr, logger, nil)

	t.Log("Adding test ruleset to cache")
	testRules := "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\""

	cache.Put("test-instance", testRules, dataFile)

	t.Log("Requesting ruleset from server")
	req := httptest.NewRequest(http.MethodGet, "/rules/test-instance", nil)
	w := httptest.NewRecorder()
	server.handleRules(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	t.Log("Decoding response")
	var response RuleSetEntry
	err := json.NewDecoder(w.Body).Decode(&response)
	require.NoError(t, err)

	t.Log("Verifying response contents")
	assert.NotEmpty(t, response.UUID)
	assert.NotEmpty(t, response.Timestamp)
	assert.Equal(t, testRules, response.Rules)
	assert.Equal(t, dataFile, response.DataFiles)

	t.Log("do with null datafile")
	w = httptest.NewRecorder()
	cache.Put("test-instance", testRules, nil)

	t.Log("Requesting ruleset from server")
	server.handleRules(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	t.Log("Decoding response")
	var response2 RuleSetEntry
	err = json.NewDecoder(w.Body).Decode(&response2)
	require.NoError(t, err)

	t.Log("Verifying response contents")
	assert.NotEmpty(t, response2.UUID)
	assert.NotEmpty(t, response2.Timestamp)
	assert.Equal(t, testRules, response2.Rules)
	assert.Empty(t, response2.DataFiles)
}

func TestServer_HandleLatest_Success(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, testServerAddr, logger, nil)

	t.Log("Adding test ruleset to cache")
	cache.Put("test-instance", "test rules", dataFile)

	t.Log("Requesting latest from server")
	req := httptest.NewRequest(http.MethodGet, "/rules/test-instance/latest", nil)
	w := httptest.NewRecorder()
	server.handleRules(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	t.Log("Decoding response")
	var response LatestResponse
	err := json.NewDecoder(w.Body).Decode(&response)
	require.NoError(t, err)

	t.Log("Verifying response contents")
	assert.NotEmpty(t, response.UUID)
	assert.NotEmpty(t, response.Timestamp)
	_, err = time.Parse(TimestampFormat, response.Timestamp)
	assert.NoError(t, err, "Timestamp should be in RFC3339Nano format")
}

func TestServer_HandleRules_UUIDConsistency(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, testServerAddr, logger, nil)

	t.Log("Adding test ruleset to cache")
	cache.Put("test-instance", "test rules", nil)

	t.Log("Requesting latest and rules endpoints")
	req1 := httptest.NewRequest(http.MethodGet, "/rules/test-instance/latest", nil)
	w1 := httptest.NewRecorder()
	server.handleRules(w1, req1)

	t.Log("Decoding latest response")
	var latestResp LatestResponse
	require.NoError(t, json.NewDecoder(w1.Body).Decode(&latestResp))

	t.Log("Requesting rules endpoint")
	req2 := httptest.NewRequest(http.MethodGet, "/rules/test-instance", nil)
	w2 := httptest.NewRecorder()
	server.handleRules(w2, req2)

	t.Log("Decoding rules response")
	var rulesResp RuleSetEntry
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&rulesResp))

	t.Log("Verifying UUID and Timestamp consistency")
	assert.Equal(t, latestResp.UUID, rulesResp.UUID)
	assert.Equal(t, latestResp.Timestamp, rulesResp.Timestamp.Format(TimestampFormat))
}

func TestServer_GCByAge(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)

	t.Log("Using very short intervals for testing")
	gc := &GarbageCollectionConfig{
		GCInterval: 50 * time.Millisecond,
		MaxAge:     100 * time.Millisecond,
		MaxSize:    1024 * 1024 * 1024, // 1GB - disable size-based pruning
	}
	server := NewServer(cache, testServerAddr, logger, gc)

	modifiedData := make(map[string][]byte)
	maps.Copy(modifiedData, testDataFile)
	modifiedData["anotherfile.data"] = []byte("another content")

	t.Log("Starting the GC")
	ctx := t.Context()
	go server.rungc(ctx)

	t.Log("Adding entries across multiple instances")
	cache.Put("instance1", "instance1 old", nil)
	cache.Put("instance1", "instance1 new", nil)
	cache.Put("instance2", "instance2 old", nil)
	cache.Put("instance2", "instance2 new", nil)
	cache.Put("instance3", "only version", nil)
	cache.Put("instancedata", "instancedata old", testDataFile)
	cache.Put("instancedata", "instancedata new", modifiedData)

	t.Log("Manually marking old entries with old timestamps")
	cache.SetEntryTimestamp("instance1", 0, time.Now().Add(-200*time.Millisecond))
	cache.SetEntryTimestamp("instance2", 0, time.Now().Add(-200*time.Millisecond))
	cache.SetEntryTimestamp("instance3", 0, time.Now().Add(-50*time.Millisecond))
	cache.SetEntryTimestamp("instancedata", 0, time.Now().Add(-200*time.Millisecond))

	initialTotal := cache.CountEntries("instance1") + cache.CountEntries("instance2") + cache.CountEntries("instancedata")
	t.Logf("Initial entries for instance1 and instance2: %d", initialTotal)

	t.Log("Waiting for a coupleGC cycles to complete")
	time.Sleep(150 * time.Millisecond)

	t.Log("Verifying latest entries still exist")
	entry1, ok1 := cache.Get("instance1")
	assert.True(t, ok1)
	assert.Equal(t, "instance1 new", entry1.Rules)
	entry2, ok2 := cache.Get("instance2")
	assert.True(t, ok2)
	assert.Equal(t, "instance2 new", entry2.Rules)
	entry3, ok3 := cache.Get("instance3")
	assert.True(t, ok3)
	assert.Equal(t, "only version", entry3.Rules)
	entrydatafile, okdata := cache.Get("instancedata")
	assert.True(t, okdata)
	assert.Equal(t, "instancedata new", entrydatafile.Rules)
	assert.Equal(t, modifiedData, entrydatafile.DataFiles)

	t.Log("Verifying old entries were pruned")
	assert.Equal(t, 1, cache.CountEntries("instance1"), "instance1 should have only latest entry")
	assert.Equal(t, 1, cache.CountEntries("instance2"), "instance2 should have only latest entry")
	assert.Equal(t, 1, cache.CountEntries("instance3"), "instance3 entry is recent enough to keep")
	assert.Equal(t, 1, cache.CountEntries("instancedata"), "instancedata should have only latest entry")
}

func TestServer_GCBySize(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)

	t.Log("Setting very small max size for testing")
	gc := &GarbageCollectionConfig{
		GCInterval: 50 * time.Millisecond,
		MaxAge:     24 * time.Hour, // disable age-based pruning
		MaxSize:    50,
	}
	server := NewServer(cache, ":0", logger, gc)

	t.Log("Adding multiple versions for some instances to create prunable entries")
	cache.Put("instance1", "instance1 old - 27 chars...", nil)
	cache.Put("instance1", "instance1 new - 27 chars...", nil)
	cache.Put("instance2", "instance2 old - 27 chars...", nil)
	cache.Put("instance2", "instance2 new - 27 chars...", nil)
	cache.Put("instance3", "instance3 - 25 characters..", nil)

	t.Log("Adding large entry that alone exceeds max size (edge case)")
	largeRules := "This is a large ruleset that exceeds the max size limit by itself"
	cache.Put("instance4", largeRules, nil)

	initialSize := cache.TotalSize()
	initialCount := cache.CountEntries("instance1") + cache.CountEntries("instance2") + cache.CountEntries("instance3") + cache.CountEntries("instance4")
	t.Logf("Initial: %d total entries, %d bytes (max: %d)", initialCount, initialSize, gc.MaxSize)
	assert.Greater(t, initialSize, gc.MaxSize, "Test setup: initial size should exceed max")
	assert.Equal(t, 6, initialCount, "Should have 6 total entries (2+2+1+1)")

	t.Log("Starting GC")
	ctx := t.Context()
	go server.rungc(ctx)

	t.Log("Waiting for a coupleGC cycles to complete")
	time.Sleep(150 * time.Millisecond)
	finalSize := cache.TotalSize()
	finalCount := cache.CountEntries("instance1") + cache.CountEntries("instance2") + cache.CountEntries("instance3") + cache.CountEntries("instance4")
	t.Logf("Final: %d total entries, %d bytes", finalCount, finalSize)

	t.Log("Verifying all latest entries still exist")
	entry1, ok1 := cache.Get("instance1")
	require.True(t, ok1, "instance1 should exist")
	assert.Equal(t, "instance1 new - 27 chars...", entry1.Rules)
	entry2, ok2 := cache.Get("instance2")
	require.True(t, ok2, "instance2 should exist")
	assert.Equal(t, "instance2 new - 27 chars...", entry2.Rules)
	entry3, ok3 := cache.Get("instance3")
	require.True(t, ok3, "instance3 should exist")
	assert.Equal(t, "instance3 - 25 characters..", entry3.Rules)
	entry4, ok4 := cache.Get("instance4")
	require.True(t, ok4, "instance4 should exist even though it exceeds max")
	assert.Equal(t, largeRules, entry4.Rules)

	t.Log("Verifying old versions were pruned")
	assert.Equal(t, 4, finalCount, "Should have 4 entries after pruning (1+1+1+1)")
	assert.Less(t, finalCount, initialCount, "Should have pruned old versions")
	assert.Less(t, finalSize, initialSize, "Cache size should be reduced")

	t.Log("Verifying cache size still exceeds max due to large entry (expected due to protected entries)")
	assert.Greater(t, finalSize, gc.MaxSize, "Cache size exceeds max")
}

func TestServer_GCEmptyCache(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)

	gc := &GarbageCollectionConfig{
		GCInterval: 50 * time.Millisecond,
		MaxAge:     100 * time.Millisecond,
		MaxSize:    100,
	}
	server := NewServer(cache, ":0", logger, gc)

	t.Log("Starting GC on empty cache")
	ctx := t.Context()
	go server.rungc(ctx)

	t.Log("Waiting for multiple GC cycles")
	time.Sleep(200 * time.Millisecond)

	t.Log("Verifying cache is still empty and no errors occurred")
	assert.Equal(t, 0, cache.TotalSize())
	assert.Empty(t, cache.ListKeys())
}
func TestServer_HandleGetRules_NotFound(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, testServerAddr, logger, nil)
	req := httptest.NewRequest(http.MethodGet, "/rules/non-existent", nil)
	w := httptest.NewRecorder()
	server.handleRules(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestServer_HandleGetRules_MissingInstance(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, testServerAddr, logger, nil)
	req := httptest.NewRequest(http.MethodGet, "/rules/", nil)
	w := httptest.NewRecorder()
	server.handleRules(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestServer_HandleLatest_NotFound(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, testServerAddr, logger, nil)
	req := httptest.NewRequest(http.MethodGet, "/rules/non-existent/latest", nil)
	w := httptest.NewRecorder()
	server.handleRules(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestServer_HandleRules_MethodNotAllowed(t *testing.T) {
	cache := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	server := NewServer(cache, testServerAddr, logger, nil)
	methods := []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/rules/test-instance", nil)
			w := httptest.NewRecorder()
			server.handleRules(w, req)
			assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
		})
	}
}
