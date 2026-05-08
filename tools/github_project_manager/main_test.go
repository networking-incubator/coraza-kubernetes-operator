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
	"bytes"
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
// Tests - Configuration
// -----------------------------------------------------------------------------

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		envs    map[string]string
		want    config
		wantErr string
	}{
		{
			name: "all flags on command line",
			args: []string{"--owner", "org", "--repo", "repo", "--issue", "42", "--verbose", "--dry-run", "--project", "3", "update-labels"},
			envs: map[string]string{"GITHUB_TOKEN": "tok"},
			want: config{
				verbose: true,
				dryRun:  true,
				owner:   "org",
				repo:    "repo",
				issue:   42,
				project: 3,
				command: "update-labels",
				token:   "tok",
			},
		},
		{
			name: "env var fallbacks",
			args: []string{"close-declined"},
			envs: map[string]string{
				"GITHUB_OWNER": "env-org",
				"GITHUB_REPO":  "env-repo",
				"GITHUB_ISSUE": "7",
				"GITHUB_TOKEN": "tok",
			},
			want: config{
				owner:   "env-org",
				repo:    "env-repo",
				issue:   7,
				project: 0,
				command: "close-declined",
				token:   "tok",
			},
		},
		{
			name: "flags override env vars",
			args: []string{"--owner", "flag-org", "--repo", "flag-repo", "--issue", "10", "triage-pr"},
			envs: map[string]string{
				"GITHUB_OWNER": "env-org",
				"GITHUB_REPO":  "env-repo",
				"GITHUB_ISSUE": "99",
				"GITHUB_TOKEN": "tok",
			},
			want: config{
				owner:   "flag-org",
				repo:    "flag-repo",
				issue:   10,
				project: 0,
				command: "triage-pr",
				token:   "tok",
			},
		},
		{
			name:    "missing command",
			args:    []string{"--owner", "o", "--repo", "r", "--issue", "1"},
			envs:    map[string]string{"GITHUB_TOKEN": "tok"},
			wantErr: "missing command",
		},
		{
			name:    "invalid GITHUB_ISSUE env",
			args:    []string{"update-labels"},
			envs:    map[string]string{"GITHUB_OWNER": "o", "GITHUB_REPO": "r", "GITHUB_ISSUE": "notanumber", "GITHUB_TOKEN": "tok"},
			wantErr: "invalid GITHUB_ISSUE",
		},
		{
			name:    "missing required fields",
			args:    []string{"update-labels"},
			envs:    map[string]string{"GITHUB_TOKEN": "tok"},
			wantErr: "--owner, --repo, and --issue are required",
		},
		{
			name:    "missing GITHUB_TOKEN",
			args:    []string{"--owner", "o", "--repo", "r", "--issue", "1", "update-labels"},
			wantErr: "GITHUB_TOKEN environment variable is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envs {
				t.Setenv(k, v)
			}

			got, err := parseConfig(tt.args)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewLogger(t *testing.T) {
	t.Run("silent when not verbose and not dry-run", func(t *testing.T) {
		log := newLogger(config{})
		// Should not panic; output goes to stdout (not captured here).
		log("should be silent")
	})

	t.Run("prints when verbose", func(t *testing.T) {
		log := newLogger(config{verbose: true})
		log("visible %d", 1)
	})

	t.Run("prints when dry-run", func(t *testing.T) {
		log := newLogger(config{dryRun: true})
		log("visible %d", 2)
	})
}

// -----------------------------------------------------------------------------
// Tests - Commands
// -----------------------------------------------------------------------------

func TestDispatch(t *testing.T) {
	t.Run("unknown command returns error", func(t *testing.T) {
		client := newTestClient(t, jsonHandler(t, Issue{}))
		cfg := config{command: "bogus", issue: 1, dryRun: true}

		err := dispatch(cfg, client)

		require.Error(t, err)
		assert.Contains(t, err.Error(), `unknown command "bogus"`)
	})

	t.Run("update-labels routes correctly", func(t *testing.T) {
		iss := Issue{
			Number: 1,
			State:  "open",
			Labels: []string{"triage/accepted"},
		}
		iss.Milestone = &struct{}{}
		client := newTestClient(t, issueHandler(t, iss))
		cfg := config{command: "update-labels", issue: 1, dryRun: true}

		err := dispatch(cfg, client)

		require.NoError(t, err)
	})

	t.Run("close-declined routes correctly", func(t *testing.T) {
		iss := Issue{Number: 1, State: "open", Labels: []string{"bug"}}
		client := newTestClient(t, issueHandler(t, iss))
		cfg := config{command: "close-declined", issue: 1, dryRun: true}

		err := dispatch(cfg, client)

		require.NoError(t, err)
	})
}

func TestRunUpdateLabels(t *testing.T) {
	t.Run("declined issue is skipped", func(t *testing.T) {
		err := runUpdateLabels(nil, 1, []string{"triage/declined"}, false, "", false, noopLogger)

		require.NoError(t, err)
	})

	t.Run("no changes needed", func(t *testing.T) {
		err := runUpdateLabels(nil, 1, []string{"triage/accepted", "size/M", "area/testing"}, true, "", false, noopLogger)

		require.NoError(t, err)
	})

	t.Run("adds needs-triage when no milestone and no triage label", func(t *testing.T) {
		var addedLabels []string
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels") {
				var body map[string][]string
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				addedLabels = body["labels"]
				writeJSON(w, []map[string]string{})
				return
			}
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		})

		err := runUpdateLabels(client, 1, []string{}, false, "", false, noopLogger)

		require.NoError(t, err)
		assert.Contains(t, addedLabels, "triage/needs-triage")
	})

	t.Run("dry-run skips API calls", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("unexpected API call in dry-run: %s %s", r.Method, r.URL.Path)
		})

		err := runUpdateLabels(client, 1, []string{}, false, "", true, noopLogger)

		require.NoError(t, err)
	})
}

func TestRunCloseDeclined(t *testing.T) {
	t.Run("not declined is a no-op", func(t *testing.T) {
		err := runCloseDeclined(nil, 1, []string{"bug"}, false, "open", false, noopLogger)

		require.NoError(t, err)
	})

	t.Run("declined closes and cleans up", func(t *testing.T) {
		var (
			removedLabels []string
			patched       []string
		)
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/"):
				parts := strings.Split(r.URL.Path, "/labels/")
				removedLabels = append(removedLabels, parts[len(parts)-1])
				writeJSON(w, []map[string]string{})
			case r.Method == "PATCH":
				body, _ := io.ReadAll(r.Body)
				patched = append(patched, string(body))
				writeJSON(w, map[string]string{})
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		})

		err := runCloseDeclined(client, 1, []string{"triage/declined", "triage/needs-triage"}, true, "open", false, noopLogger)

		require.NoError(t, err)
		assert.Equal(t, []string{"triage/needs-triage"}, removedLabels)
		assert.Len(t, patched, 2) // milestone removal + close
	})

	t.Run("dry-run skips API calls", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("unexpected API call in dry-run: %s %s", r.Method, r.URL.Path)
		})

		err := runCloseDeclined(client, 1, []string{"triage/declined"}, false, "open", true, noopLogger)

		require.NoError(t, err)
	})
}

func TestRunTriagePR(t *testing.T) {
	t.Run("dry-run computes labels and milestone without API mutations", func(t *testing.T) {
		var getCalls int
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/1"):
				getCalls++
				writeJSON(w, pullRequestInfo{NodeID: "node1", Additions: 5, Deletions: 3})
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/1/files"):
				getCalls++
				writeJSON(w, []map[string]string{{"filename": "api/v1alpha1/types.go"}})
			case r.Method == "GET" && strings.Contains(r.URL.Path, "/milestones"):
				getCalls++
				writeJSON(w, []Milestone{{Number: 1, Title: "v0.1.0"}})
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}

		client := newTestClient(t, handler)
		iss := &Issue{Number: 1, State: "open", Labels: []string{}}

		err := runTriagePR(client, 1, iss, 1, true, io.Discard, noopLogger)

		require.NoError(t, err)
		assert.Equal(t, 3, getCalls)
	})

	t.Run("auto-discovers project when project is 0", func(t *testing.T) {
		var graphQLCalls []string
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/1"):
				writeJSON(w, pullRequestInfo{NodeID: "node1", Additions: 5, Deletions: 3})
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/1/files"):
				writeJSON(w, []map[string]string{{"filename": "api/v1alpha1/types.go"}})
			case r.Method == "GET" && strings.Contains(r.URL.Path, "/milestones"):
				writeJSON(w, []Milestone{{Number: 1, Title: "v0.1.0"}})
			case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
				writeJSON(w, []map[string]string{})
			case r.Method == "PATCH":
				writeJSON(w, map[string]string{})
			case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/graphql"):
				body, _ := io.ReadAll(r.Body)
				var req struct{ Query string }
				json.Unmarshal(body, &req)
				graphQLCalls = append(graphQLCalls, req.Query)

				if strings.Contains(req.Query, "projectsV2") {
					writeJSON(w, map[string]any{
						"data": map[string]any{
							"organization": map[string]any{
								"projectsV2": map[string]any{
									"nodes": []map[string]any{
										{"id": "PVT_1", "number": 1, "closed": false},
									},
								},
							},
						},
					})
				} else if strings.Contains(req.Query, "addProjectV2ItemById") {
					writeJSON(w, map[string]any{
						"data": map[string]any{
							"addProjectV2ItemById": map[string]any{
								"item": map[string]any{"id": "PVTI_1"},
							},
						},
					})
				} else if strings.Contains(req.Query, "node") {
					writeJSON(w, map[string]any{
						"data": map[string]any{
							"node": map[string]any{
								"field": map[string]any{
									"id": "FIELD_1",
									"options": []map[string]any{
										{"id": "OPT_1", "name": "Review"},
									},
								},
							},
						},
					})
				} else if strings.Contains(req.Query, "updateProjectV2ItemFieldValue") {
					writeJSON(w, map[string]any{
						"data": map[string]any{
							"updateProjectV2ItemFieldValue": map[string]any{
								"projectV2Item": map[string]any{"id": "PVTI_1"},
							},
						},
					})
				}
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}

		client := newTestClient(t, handler)
		iss := &Issue{Number: 1, State: "open", Labels: []string{}}

		err := runTriagePR(client, 1, iss, 0, false, io.Discard, noopLogger)

		require.NoError(t, err)
		require.NotEmpty(t, graphQLCalls)
		assert.True(t, strings.Contains(graphQLCalls[0], "projectsV2"), "first GraphQL call should be project discovery")
	})

	t.Run("project board failure emits warning to stderr", func(t *testing.T) {
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/1"):
				writeJSON(w, pullRequestInfo{NodeID: "node1", Additions: 5, Deletions: 3})
			case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/1/files"):
				writeJSON(w, []map[string]string{{"filename": "api/v1alpha1/types.go"}})
			case r.Method == "GET" && strings.Contains(r.URL.Path, "/milestones"):
				writeJSON(w, []Milestone{{Number: 1, Title: "v0.1.0"}})
			case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
				writeJSON(w, []map[string]string{})
			case r.Method == "PATCH":
				writeJSON(w, map[string]string{})
			case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/graphql"):
				// Fail all GraphQL calls to simulate project board failure
				w.WriteHeader(http.StatusBadGateway)
				w.Write([]byte("bad gateway"))
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}

		client := newTestClient(t, handler)
		iss := &Issue{Number: 1, State: "open", Labels: []string{}}

		var buf bytes.Buffer
		triageErr := runTriagePR(client, 1, iss, 1, false, &buf, noopLogger)

		require.NoError(t, triageErr, "project board failure should not be a fatal error")
		assert.Contains(t, buf.String(), "::warning::")
	})
}

// -----------------------------------------------------------------------------
// Tests - Helpers
// -----------------------------------------------------------------------------

func TestApplyLabels(t *testing.T) {
	t.Run("adds and removes labels", func(t *testing.T) {
		var addedLabels []string
		var removedLabels []string
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
				var body map[string][]string
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				addedLabels = body["labels"]
				writeJSON(w, []map[string]string{})
			case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/"):
				parts := strings.Split(r.URL.Path, "/labels/")
				removedLabels = append(removedLabels, parts[len(parts)-1])
				writeJSON(w, []map[string]string{})
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		})

		err := applyLabels(client, 1, []string{"area/testing"}, []string{"triage/needs-triage"}, false, noopLogger)

		require.NoError(t, err)
		assert.Equal(t, []string{"area/testing"}, addedLabels)
		assert.Equal(t, []string{"triage/needs-triage"}, removedLabels)
	})

	t.Run("dry-run skips API calls", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("unexpected API call: %s %s", r.Method, r.URL.Path)
		})

		err := applyLabels(client, 1, []string{"bug"}, []string{"wontfix"}, true, noopLogger)

		require.NoError(t, err)
	})

	t.Run("empty lists are a no-op", func(t *testing.T) {
		err := applyLabels(nil, 1, nil, nil, false, noopLogger)

		require.NoError(t, err)
	})
}

func TestAssignMilestone(t *testing.T) {
	t.Run("assigns lowest semver milestone", func(t *testing.T) {
		var patchedMilestone int
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "GET" && strings.Contains(r.URL.Path, "/milestones"):
				writeJSON(w, []Milestone{
					{Number: 2, Title: "v1.0.0"},
					{Number: 1, Title: "v0.5.0"},
				})
			case r.Method == "PATCH":
				var body map[string]int
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				patchedMilestone = body["milestone"]
				writeJSON(w, map[string]string{})
			default:
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		})

		err := assignMilestone(client, 1, false, noopLogger)

		require.NoError(t, err)
		assert.Equal(t, 1, patchedMilestone)
	})

	t.Run("no valid milestones logs skip", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, []Milestone{{Number: 1, Title: "backlog"}})
		})

		err := assignMilestone(client, 1, false, noopLogger)

		require.NoError(t, err)
	})

	t.Run("dry-run skips API mutation", func(t *testing.T) {
		client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				writeJSON(w, []Milestone{{Number: 1, Title: "v1.0.0"}})
				return
			}
			t.Fatalf("unexpected mutation: %s %s", r.Method, r.URL.Path)
		})

		err := assignMilestone(client, 1, true, noopLogger)

		require.NoError(t, err)
	})
}

func TestUsage(t *testing.T) {
	out := usage()

	assert.Contains(t, out, "update-labels")
	assert.Contains(t, out, "close-declined")
	assert.Contains(t, out, "triage-pr")
	assert.Contains(t, out, "GITHUB_TOKEN")
}

// -----------------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------------

func noopLogger(string, ...any) {}

func newTestClient(t *testing.T, handler http.HandlerFunc) *GitHubClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewGitHubClient("test-token", "owner", "repo")
	client.baseURL = srv.URL
	return client
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// jsonHandler returns a handler that serves the same JSON response for all requests.
func jsonHandler(t *testing.T, v any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, v)
	}
}

// issueHandler returns a handler that serves an Issue for GET requests to the
// issue endpoint, and accepts all other requests with an empty JSON response.
func issueHandler(t *testing.T, iss Issue) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/issues/") {
			// Encode as the raw GitHub format (labels as objects).
			type labelObj struct {
				Name string `json:"name"`
			}
			raw := struct {
				Number    int        `json:"number"`
				State     string     `json:"state"`
				Body      string     `json:"body"`
				Labels    []labelObj `json:"labels"`
				Milestone *struct{}  `json:"milestone"`
			}{
				Number:    iss.Number,
				State:     iss.State,
				Body:      iss.Body,
				Milestone: iss.Milestone,
			}
			for _, l := range iss.Labels {
				raw.Labels = append(raw.Labels, labelObj{Name: l})
			}
			writeJSON(w, raw)
			return
		}
		writeJSON(w, map[string]string{})
	}
}
