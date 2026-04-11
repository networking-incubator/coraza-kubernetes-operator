//go:build integration

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

package integration

// TODO: move this into perf test suite when ready: https://github.com/networking-incubator/coraza-kubernetes-operator/issues/115

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestHighThroughput sends many concurrent requests to verify that
// WAF rules are consistently enforced under load.
func TestHighThroughput(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("high-throughput")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy rules")
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
	s.CreateConfigMap(ns, "block-rules", framework.SimpleBlockRule(12001, "blocked"))
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	// Wait for WAF to be ready
	gw.ExpectBlocked("/?test=blocked")
	gw.ExpectAllowed("/?test=safe")

	s.Step("send 100 concurrent blocked requests")
	var blockedCount atomic.Int32
	var blockedErrors atomic.Int32

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result := gw.Get(fmt.Sprintf("/?test=blocked&req=%d", i))
			if result.Err != nil {
				blockedErrors.Add(1)
				return
			}
			if result.StatusCode == http.StatusForbidden {
				blockedCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	t.Logf("Blocked requests: %d/100, errors: %d", blockedCount.Load(), blockedErrors.Load())
	assert.GreaterOrEqual(t, blockedCount.Load(), int32(95), "At least 95% of blocked requests should return 403")

	s.Step("send 100 concurrent allowed requests")
	var allowedCount atomic.Int32
	var allowedErrors atomic.Int32

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result := gw.Get(fmt.Sprintf("/?test=safe&req=%d", i))
			if result.Err != nil {
				allowedErrors.Add(1)
				return
			}
			if result.StatusCode == http.StatusOK {
				allowedCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	t.Logf("Allowed requests: %d/100, errors: %d", allowedCount.Load(), allowedErrors.Load())
	assert.GreaterOrEqual(t, allowedCount.Load(), int32(95), "At least 95% of allowed requests should return 200")
}

// TestMixedTrafficLoad sends a mix of blocked and allowed requests
// concurrently to verify consistent WAF behavior.
func TestMixedTrafficLoad(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("mixed-load")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy multiple rules")
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
	s.CreateConfigMap(ns, "block-rules", `
SecRule ARGS:attack "@contains sqli" "id:12101,phase:2,deny,status:403,msg:'SQL injection'"
SecRule ARGS:attack "@contains xss" "id:12102,phase:2,deny,status:403,msg:'XSS'"
SecRule ARGS:attack "@contains rce" "id:12103,phase:2,deny,status:403,msg:'RCE'"
`)
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")

	// Wait for WAF to be ready
	gw.ExpectBlocked("/?attack=sqli")
	gw.ExpectAllowed("/?safe=true")

	s.Step("send mixed traffic")

	type result struct {
		path         string
		expected     int
		actual       int
		err          error
		responseTime time.Duration
	}

	requests := []struct {
		path     string
		expected int
	}{
		{"/?attack=sqli", http.StatusForbidden},
		{"/?attack=xss", http.StatusForbidden},
		{"/?attack=rce", http.StatusForbidden},
		{"/?safe=true", http.StatusOK},
		{"/?param=value", http.StatusOK},
		{"/api/data", http.StatusOK},
	}

	var wg sync.WaitGroup
	results := make(chan result, len(requests)*20)

	// Send each request type 20 times concurrently
	for _, req := range requests {
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(path string, expected int) {
				defer wg.Done()
				start := time.Now()
				r := gw.Get(path)
				results <- result{
					path:         path,
					expected:     expected,
					actual:       r.StatusCode,
					err:          r.Err,
					responseTime: time.Since(start),
				}
			}(req.path, req.expected)
		}
	}

	wg.Wait()
	close(results)

	s.Step("analyze results")
	var correct, incorrect, errors int
	var totalTime time.Duration

	for r := range results {
		if r.err != nil {
			errors++
			continue
		}
		totalTime += r.responseTime
		if r.actual == r.expected {
			correct++
		} else {
			incorrect++
			t.Logf("Unexpected result: %s expected %d, got %d", r.path, r.expected, r.actual)
		}
	}

	total := correct + incorrect + errors
	t.Logf("Results: %d/%d correct (%.1f%%), %d errors, avg response time: %v",
		correct, total, float64(correct)/float64(total)*100,
		errors, totalTime/time.Duration(total))

	require.GreaterOrEqual(t, float64(correct)/float64(total), 0.95,
		"At least 95%% of requests should have expected response")
}

// TestSustainedLoad sends requests over a sustained period to verify
// WAF stability over time.
func TestSustainedLoad(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("sustained-load")

	s.Step("create gateway")
	s.CreateGateway(ns, "gw")
	s.ExpectGatewayProgrammed(ns, "gw")

	s.Step("deploy rules")
	s.CreateConfigMap(ns, "base-rules", `SecRuleEngine On`)
	s.CreateConfigMap(ns, "block-rules", framework.SimpleBlockRule(12201, "blocked"))
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-rules"})

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", "gw", "echo")

	gw := s.ProxyToGateway(ns, "gw")
	gw.ExpectBlocked("/?test=blocked")

	s.Step("send sustained load for 10 seconds")
	duration := 10 * time.Second
	deadline := time.Now().Add(duration)

	var blockedCorrect, allowedCorrect, total atomic.Int32
	var wg sync.WaitGroup

	// Worker pool of 10 concurrent requesters
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				total.Add(1)

				// Alternate between blocked and allowed requests
				if total.Load()%2 == 0 {
					result := gw.Get("/?test=blocked")
					if result.Err == nil && result.StatusCode == http.StatusForbidden {
						blockedCorrect.Add(1)
					}
				} else {
					result := gw.Get("/?test=safe")
					if result.Err == nil && result.StatusCode == http.StatusOK {
						allowedCorrect.Add(1)
					}
				}
			}
		}()
	}

	wg.Wait()

	correctTotal := blockedCorrect.Load() + allowedCorrect.Load()
	t.Logf("Sustained load: %d total requests, %d correct (%.1f%%), blocked=%d, allowed=%d",
		total.Load(), correctTotal, float64(correctTotal)/float64(total.Load())*100,
		blockedCorrect.Load(), allowedCorrect.Load())

	assert.GreaterOrEqual(t, float64(correctTotal)/float64(total.Load()), 0.99,
		"At least 99%% of requests should have correct response under sustained load")
}
