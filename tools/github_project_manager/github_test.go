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

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Tests - Issue / Milestone types
// -----------------------------------------------------------------------------

func TestIssueUnmarshalJSON(t *testing.T) {
	t.Run("flattens label objects to strings", func(t *testing.T) {
		raw := `{"number":1,"state":"open","body":"desc","labels":[{"name":"bug"},{"name":"area/api"}],"milestone":{"id":1}}`

		var iss Issue
		require.NoError(t, json.Unmarshal([]byte(raw), &iss))

		assert.Equal(t, 1, iss.Number)
		assert.Equal(t, "open", iss.State)
		assert.Equal(t, "desc", iss.Body)
		assert.Equal(t, []string{"bug", "area/api"}, iss.Labels)
		assert.True(t, iss.HasMilestone())
	})

	t.Run("no milestone", func(t *testing.T) {
		raw := `{"number":2,"state":"closed","labels":[],"milestone":null}`

		var iss Issue
		require.NoError(t, json.Unmarshal([]byte(raw), &iss))

		assert.False(t, iss.HasMilestone())
		assert.Empty(t, iss.Labels)
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		var iss Issue
		err := json.Unmarshal([]byte(`{invalid`), &iss)

		require.Error(t, err)
	})
}

// -----------------------------------------------------------------------------
// Tests - GitHubClient REST endpoints
// -----------------------------------------------------------------------------

func TestGetIssue(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "GET", r.Method)
			assert.Contains(t, r.URL.Path, "/issues/5")
			writeJSON(w, map[string]any{
				"number": 5, "state": "open", "body": "test",
				"labels": []map[string]string{{"name": "bug"}},
			})
		})

		iss, err := client.GetIssue(5)

		require.NoError(t, err)
		assert.Equal(t, 5, iss.Number)
		assert.Equal(t, []string{"bug"}, iss.Labels)
	})

	t.Run("server error", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("fail"))
		})

		_, err := client.GetIssue(5)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "fetching issue #5")
	})
}

func TestAddLabels(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var received map[string][]string
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "POST", r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
			writeJSON(w, []map[string]string{})
		})

		err := client.AddLabels(1, []string{"bug", "area/api"})

		require.NoError(t, err)
		assert.Equal(t, []string{"bug", "area/api"}, received["labels"])
	})

	t.Run("non-200 returns error", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte("validation failed"))
		})

		err := client.AddLabels(1, []string{"bad"})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "adding labels to #1")
	})
}

func TestRemoveLabel(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "DELETE", r.Method)
			writeJSON(w, []map[string]string{})
		})

		err := client.RemoveLabel(1, "bug")

		require.NoError(t, err)
	})

	t.Run("404 is not an error", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})

		err := client.RemoveLabel(1, "gone")

		require.NoError(t, err)
	})

	t.Run("500 returns error", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("fail"))
		})

		err := client.RemoveLabel(1, "bug")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "removing label")
	})
}

func TestCloseIssue(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var body string
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "PATCH", r.Method)
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			writeJSON(w, map[string]string{})
		})

		err := client.CloseIssue(1)

		require.NoError(t, err)
		assert.Contains(t, body, `"state":"closed"`)
	})

	t.Run("error", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("fail"))
		})

		err := client.CloseIssue(1)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "closing issue #1")
	})
}

func TestRemoveMilestone(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var body string
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			writeJSON(w, map[string]string{})
		})

		err := client.RemoveMilestone(1)

		require.NoError(t, err)
		assert.Contains(t, body, `"milestone":null`)
	})
}

func TestListOpenMilestones(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Contains(t, r.URL.RawQuery, "state=open")
			writeJSON(w, []Milestone{{Number: 1, Title: "v1.0.0"}, {Number: 2, Title: "v2.0.0"}})
		})

		milestones, err := client.ListOpenMilestones()

		require.NoError(t, err)
		assert.Len(t, milestones, 2)
		assert.Equal(t, "v1.0.0", milestones[0].Title)
	})
}

func TestSetMilestone(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var body map[string]int
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			writeJSON(w, map[string]string{})
		})

		err := client.SetMilestone(1, 42)

		require.NoError(t, err)
		assert.Equal(t, 42, body["milestone"])
	})
}

func TestGetPullRequestInfo(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Contains(t, r.URL.Path, "/pulls/3")
			writeJSON(w, pullRequestInfo{NodeID: "PR_123", Additions: 10, Deletions: 5})
		})

		info, err := client.GetPullRequestInfo(3)

		require.NoError(t, err)
		assert.Equal(t, "PR_123", info.NodeID)
		assert.Equal(t, 10, info.Additions)
		assert.Equal(t, 5, info.Deletions)
	})
}

func TestGetPullRequestFiles(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, []map[string]string{
				{"filename": "api/types.go"},
				{"filename": "internal/controller/engine.go"},
			})
		})

		files, err := client.GetPullRequestFiles(3)

		require.NoError(t, err)
		assert.Equal(t, []string{"api/types.go", "internal/controller/engine.go"}, files)
	})
}

// -----------------------------------------------------------------------------
// Tests - GitHubClient GraphQL / Project Board
// -----------------------------------------------------------------------------

func TestAddToProjectBoard(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var callCount int
		client := newTestClient(t, graphQLRouter(t, map[string]any{
			"organization": graphQLData{
				data: `{"organization":{"projectV2":{"id":"PVT_org"}}}`,
			},
			"addProjectV2ItemById": graphQLData{
				data: `{"addProjectV2ItemById":{"item":{"id":"PVTI_item1"}}}`,
			},
			"node": graphQLData{
				data: `{"node":{"field":{"id":"FIELD_1","options":[{"id":"OPT_1","name":"Todo"},{"id":"OPT_2","name":"Review"}]}}}`,
			},
			"updateProjectV2ItemFieldValue": graphQLData{
				data: `{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_item1"}}}`,
			},
		}, &callCount))

		err := client.AddToProjectBoard("PR_node", 1, "Review")

		require.NoError(t, err)
		assert.Equal(t, 4, callCount)
	})

	t.Run("org lookup fails falls back to user", func(t *testing.T) {
		var callCount int
		client := newTestClient(t, graphQLRouter(t, map[string]any{
			"organization": graphQLData{
				err: "not found",
			},
			"user": graphQLData{
				data: `{"user":{"projectV2":{"id":"PVT_user"}}}`,
			},
			"addProjectV2ItemById": graphQLData{
				data: `{"addProjectV2ItemById":{"item":{"id":"PVTI_item1"}}}`,
			},
			"node": graphQLData{
				data: `{"node":{"field":{"id":"FIELD_1","options":[{"id":"OPT_1","name":"Review"}]}}}`,
			},
			"updateProjectV2ItemFieldValue": graphQLData{
				data: `{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_item1"}}}`,
			},
		}, &callCount))

		err := client.AddToProjectBoard("PR_node", 1, "Review")

		require.NoError(t, err)
		assert.Equal(t, 5, callCount) // org (fail) + user + add + lookup + update
	})

	t.Run("status option not found returns error", func(t *testing.T) {
		var callCount int
		client := newTestClient(t, graphQLRouter(t, map[string]any{
			"organization": graphQLData{
				data: `{"organization":{"projectV2":{"id":"PVT_org"}}}`,
			},
			"addProjectV2ItemById": graphQLData{
				data: `{"addProjectV2ItemById":{"item":{"id":"PVTI_item1"}}}`,
			},
			"node": graphQLData{
				data: `{"node":{"field":{"id":"FIELD_1","options":[{"id":"OPT_1","name":"Todo"}]}}}`,
			},
		}, &callCount))

		err := client.AddToProjectBoard("PR_node", 1, "Review")

		require.Error(t, err)
		assert.Contains(t, err.Error(), `status option "Review" not found`)
	})
}

func TestFindOldestOpenProject(t *testing.T) {
	t.Run("picks lowest numbered open project from org", func(t *testing.T) {
		var callCount int
		client := newTestClient(t, graphQLRouter(t, map[string]any{
			"organization": graphQLData{
				data: `{"organization":{"projectsV2":{"nodes":[
					{"id":"PVT_3","number":3,"closed":false},
					{"id":"PVT_1","number":1,"closed":false},
					{"id":"PVT_2","number":2,"closed":true}
				]}}}`,
			},
		}, &callCount))

		id, err := client.findOldestOpenProject()

		require.NoError(t, err)
		assert.Equal(t, "PVT_1", id)
		assert.Equal(t, 1, callCount)
	})

	t.Run("falls back to user when org query fails", func(t *testing.T) {
		var callCount int
		client := newTestClient(t, graphQLRouter(t, map[string]any{
			"organization": graphQLData{
				err: "not an organization",
			},
			"user": graphQLData{
				data: `{"user":{"projectsV2":{"nodes":[
					{"id":"PVT_5","number":5,"closed":false},
					{"id":"PVT_2","number":2,"closed":false}
				]}}}`,
			},
		}, &callCount))

		id, err := client.findOldestOpenProject()

		require.NoError(t, err)
		assert.Equal(t, "PVT_2", id)
		assert.Equal(t, 2, callCount)
	})

	t.Run("no open projects returns error", func(t *testing.T) {
		var callCount int
		client := newTestClient(t, graphQLRouter(t, map[string]any{
			"organization": graphQLData{
				data: `{"organization":{"projectsV2":{"nodes":[
					{"id":"PVT_1","number":1,"closed":true}
				]}}}`,
			},
			"user": graphQLData{
				data: `{"user":{"projectsV2":{"nodes":[
					{"id":"PVT_2","number":2,"closed":true}
				]}}}`,
			},
		}, &callCount))

		_, err := client.findOldestOpenProject()

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no open projects found")
	})

	t.Run("empty project list returns error", func(t *testing.T) {
		var callCount int
		client := newTestClient(t, graphQLRouter(t, map[string]any{
			"organization": graphQLData{
				data: `{"organization":{"projectsV2":{"nodes":[]}}}`,
			},
			"user": graphQLData{
				data: `{"user":{"projectsV2":{"nodes":[]}}}`,
			},
		}, &callCount))

		_, err := client.findOldestOpenProject()

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no open projects found")
	})
}

func TestLookupProjectID(t *testing.T) {
	t.Run("empty org ID falls through to user lookup", func(t *testing.T) {
		var callCount int
		client := newTestClient(t, graphQLRouter(t, map[string]any{
			"organization": graphQLData{
				data: `{"organization":{"projectV2":{"id":""}}}`,
			},
			"user": graphQLData{
				data: `{"user":{"projectV2":{"id":"PVT_user"}}}`,
			},
		}, &callCount))

		id, err := client.lookupProjectID(1)

		require.NoError(t, err)
		assert.Equal(t, "PVT_user", id)
		assert.Equal(t, 2, callCount)
	})

	t.Run("empty ID on both paths returns error", func(t *testing.T) {
		var callCount int
		client := newTestClient(t, graphQLRouter(t, map[string]any{
			"organization": graphQLData{
				data: `{"organization":{"projectV2":{"id":""}}}`,
			},
			"user": graphQLData{
				data: `{"user":{"projectV2":{"id":""}}}`,
			},
		}, &callCount))

		_, err := client.lookupProjectID(1)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "project #1 not found")
		assert.Equal(t, 2, callCount)
	})
}

func TestDoGraphQL(t *testing.T) {
	t.Run("GraphQL-level error", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{
				"data":   nil,
				"errors": []map[string]string{{"message": "field not found"}},
			})
		})

		_, err := client.doGraphQL("query { foo }", nil)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "field not found")
	})

	t.Run("HTTP error", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("bad gateway"))
		})

		_, err := client.doGraphQL("query { foo }", nil)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "status 502")
	})
}

func TestDoRequest(t *testing.T) {
	t.Run("sets required headers", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "application/vnd.github+json", r.Header.Get("Accept"))
			assert.Equal(t, apiVersion, r.Header.Get("X-GitHub-Api-Version"))
			assert.Equal(t, userAgent, r.Header.Get("User-Agent"))
			assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
			w.Write([]byte("ok"))
		})

		_, status, err := client.doRequestNoHeaders("GET", client.baseURL+"/test", "")

		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, status)
	})

	t.Run("sets content-type for body", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
			w.Write([]byte("ok"))
		})

		_, _, err := client.doRequestNoHeaders("POST", client.baseURL+"/test", `{"key":"val"}`)

		require.NoError(t, err)
	})

	t.Run("omits auth header when token is empty", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Empty(t, r.Header.Get("Authorization"))
			w.Write([]byte("ok"))
		})
		client.token = ""

		_, _, err := client.doRequestNoHeaders("GET", client.baseURL+"/test", "")

		require.NoError(t, err)
	})
}

// -----------------------------------------------------------------------------
// Tests - Pagination
// -----------------------------------------------------------------------------

func TestNextPageURL(t *testing.T) {
	t.Run("extracts next URL", func(t *testing.T) {
		header := `<https://api.github.com/repos/o/r/milestones?page=2>; rel="next", <https://api.github.com/repos/o/r/milestones?page=5>; rel="last"`

		got := nextPageURL(header)

		assert.Equal(t, "https://api.github.com/repos/o/r/milestones?page=2", got)
	})

	t.Run("returns empty when no next", func(t *testing.T) {
		header := `<https://api.github.com/repos/o/r/milestones?page=1>; rel="prev"`

		got := nextPageURL(header)

		assert.Equal(t, "", got)
	})

	t.Run("returns empty for empty header", func(t *testing.T) {
		got := nextPageURL("")

		assert.Equal(t, "", got)
	})
}

func TestListOpenMilestonesPagination(t *testing.T) {
	t.Run("follows Link header across pages", func(t *testing.T) {
		var requestCount int
		var srvURL string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount++
			if r.URL.Query().Get("page") == "2" {
				writeJSON(w, []Milestone{{Number: 3, Title: "v0.3.0"}})
				return
			}
			nextURL := srvURL + r.URL.Path + "?state=open&per_page=100&page=2"
			w.Header().Set("Link", `<`+nextURL+`>; rel="next"`)
			writeJSON(w, []Milestone{{Number: 1, Title: "v0.1.0"}, {Number: 2, Title: "v0.2.0"}})
		}))
		t.Cleanup(srv.Close)
		srvURL = srv.URL
		client := NewGitHubClient("tok", "owner", "repo")
		client.baseURL = srv.URL

		milestones, err := client.ListOpenMilestones()

		require.NoError(t, err)
		assert.Len(t, milestones, 3, "should collect results from both pages")
		assert.Equal(t, 2, requestCount, "should make exactly 2 requests")
		assert.Equal(t, "v0.1.0", milestones[0].Title)
		assert.Equal(t, "v0.3.0", milestones[2].Title)
	})

	t.Run("single page works without Link header", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, []Milestone{{Number: 1, Title: "v1.0.0"}})
		})

		milestones, err := client.ListOpenMilestones()

		require.NoError(t, err)
		assert.Len(t, milestones, 1)
	})
}

func TestGetPullRequestFilesPagination(t *testing.T) {
	t.Run("follows Link header across pages", func(t *testing.T) {
		var requestCount int
		var srvURL string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount++
			if r.URL.Query().Get("page") == "2" {
				writeJSON(w, []map[string]string{{"filename": "c.go"}})
				return
			}
			nextURL := srvURL + r.URL.Path + "?per_page=100&page=2"
			w.Header().Set("Link", `<`+nextURL+`>; rel="next"`)
			writeJSON(w, []map[string]string{{"filename": "a.go"}, {"filename": "b.go"}})
		}))
		t.Cleanup(srv.Close)
		srvURL = srv.URL
		client := NewGitHubClient("tok", "owner", "repo")
		client.baseURL = srv.URL

		files, err := client.GetPullRequestFiles(1)

		require.NoError(t, err)
		assert.Equal(t, []string{"a.go", "b.go", "c.go"}, files)
		assert.Equal(t, 2, requestCount, "should make exactly 2 requests")
	})
}

// -----------------------------------------------------------------------------
// Tests - Response size limit
// -----------------------------------------------------------------------------

func TestDoRequestLimitsResponseSize(t *testing.T) {
	t.Run("truncates responses larger than maxResponseSize", func(t *testing.T) {
		oversized := strings.Repeat("x", maxResponseSize+1024)
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(oversized))
		})

		body, _, err := client.doRequestNoHeaders("GET", client.baseURL+"/big", "")

		require.NoError(t, err)
		assert.Len(t, body, maxResponseSize, "response should be truncated to maxResponseSize")
	})

	t.Run("small responses are not truncated", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("ok"))
		})

		body, _, err := client.doRequestNoHeaders("GET", client.baseURL+"/small", "")

		require.NoError(t, err)
		assert.Equal(t, "ok", string(body))
	})
}

// -----------------------------------------------------------------------------
// Test helpers - GraphQL
// -----------------------------------------------------------------------------

// graphQLData holds a canned response for a GraphQL operation.
type graphQLData struct {
	data string // raw JSON for the "data" field
	err  string // if set, returned as a GraphQL error
}

// graphQLRouter returns a handler that routes GraphQL requests by matching
// operation keywords in the query string. callCount tracks total calls.
func graphQLRouter(t *testing.T, routes map[string]any, callCount *int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		*callCount++
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var req struct {
			Query string `json:"query"`
		}
		require.NoError(t, json.Unmarshal(body, &req))

		for keyword, respAny := range routes {
			if !strings.Contains(req.Query, keyword) {
				continue
			}

			resp, ok := respAny.(graphQLData)
			if !ok {
				t.Fatalf("invalid route value for %q", keyword)
			}

			w.Header().Set("Content-Type", "application/json")
			if resp.err != "" {
				json.NewEncoder(w).Encode(map[string]any{
					"data":   nil,
					"errors": []map[string]string{{"message": resp.err}},
				})
				return
			}

			// Wrap raw data JSON in the GraphQL response envelope.
			w.Write([]byte(`{"data":` + resp.data + `}`))
			return
		}

		t.Fatalf("unhandled GraphQL query: %s", req.Query)
	}
}
