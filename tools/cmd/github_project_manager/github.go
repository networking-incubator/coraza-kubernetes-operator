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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"
	userAgent      = "github_project_manager/1.0"
)

// Issue represents a GitHub issue with the fields we care about.
type Issue struct {
	Number    int       `json:"number"`
	State     string    `json:"state"`
	Body      string    `json:"body"`
	Labels    []string  `json:"-"`
	Milestone *struct{} `json:"milestone"`
}

// UnmarshalJSON implements custom unmarshaling to flatten label objects to
// a plain []string of label names.
func (i *Issue) UnmarshalJSON(data []byte) error {
	type issueAlias Issue
	aux := &struct {
		*issueAlias
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}{
		issueAlias: (*issueAlias)(i),
	}

	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}

	i.Labels = make([]string, len(aux.Labels))
	for idx, l := range aux.Labels {
		i.Labels[idx] = l.Name
	}

	return nil
}

// HasMilestone returns true if the issue has a milestone.
func (i *Issue) HasMilestone() bool {
	return i.Milestone != nil
}

// GitHubClient wraps the GitHub REST API for a specific repository.
type GitHubClient struct {
	token   string
	owner   string
	repo    string
	baseURL string
	client  *http.Client
}

// NewGitHubClient creates a new GitHubClient for the given repository.
func NewGitHubClient(token, owner, repo string) *GitHubClient {
	return &GitHubClient{
		token:   token,
		owner:   owner,
		repo:    repo,
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *GitHubClient) issueURL(number int) string {
	return fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, c.owner, c.repo, number)
}

func (c *GitHubClient) issueLabelsURL(number int) string {
	return c.issueURL(number) + "/labels"
}

func (c *GitHubClient) issueLabelURL(number int, label string) string {
	return c.issueURL(number) + "/labels/" + url.PathEscape(label)
}

func (c *GitHubClient) doRequest(method, url string, body string) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response: %w", err)
	}

	return respBody, resp.StatusCode, nil
}

// GetIssue fetches an issue by number.
func (c *GitHubClient) GetIssue(number int) (*Issue, error) {
	body, status, err := c.doRequest("GET", c.issueURL(number), "")
	if err != nil {
		return nil, fmt.Errorf("fetching issue #%d: %w", number, err)
	}

	if status != http.StatusOK {
		return nil, fmt.Errorf("fetching issue #%d: status %d: %s", number, status, string(body))
	}

	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, fmt.Errorf("decoding issue #%d: %w", number, err)
	}

	return &issue, nil
}

// AddLabels adds labels to an issue.
func (c *GitHubClient) AddLabels(number int, labels []string) error {
	payload, err := json.Marshal(map[string][]string{"labels": labels})
	if err != nil {
		return fmt.Errorf("encoding labels for issue #%d: %w", number, err)
	}
	body, status, err := c.doRequest("POST", c.issueLabelsURL(number), string(payload))
	if err != nil {
		return fmt.Errorf("adding labels to issue #%d: %w", number, err)
	}

	if status != http.StatusOK {
		return fmt.Errorf("adding labels to issue #%d: status %d: %s", number, status, string(body))
	}

	return nil
}

// RemoveLabel removes a label from an issue.
func (c *GitHubClient) RemoveLabel(number int, label string) error {
	body, status, err := c.doRequest("DELETE", c.issueLabelURL(number, label), "")
	if err != nil {
		return fmt.Errorf("removing label %q from issue #%d: %w", label, number, err)
	}

	// 200 = removed, 404 = already gone (both are fine)
	if status != http.StatusOK && status != http.StatusNotFound {
		return fmt.Errorf("removing label %q from issue #%d: status %d: %s", label, number, status, string(body))
	}

	return nil
}

// CloseIssue closes an issue.
func (c *GitHubClient) CloseIssue(number int) error {
	payload, err := json.Marshal(map[string]string{"state": "closed"})
	if err != nil {
		return fmt.Errorf("encoding close payload for issue #%d: %w", number, err)
	}

	body, status, err := c.doRequest("PATCH", c.issueURL(number), string(payload))
	if err != nil {
		return fmt.Errorf("closing issue #%d: %w", number, err)
	}

	if status != http.StatusOK {
		return fmt.Errorf("closing issue #%d: status %d: %s", number, status, string(body))
	}

	return nil
}

// RemoveMilestone removes the milestone from an issue.
func (c *GitHubClient) RemoveMilestone(number int) error {
	body, status, err := c.doRequest("PATCH", c.issueURL(number), `{"milestone":null}`)
	if err != nil {
		return fmt.Errorf("removing milestone from issue #%d: %w", number, err)
	}

	if status != http.StatusOK {
		return fmt.Errorf("removing milestone from issue #%d: status %d: %s", number, status, string(body))
	}

	return nil
}

// Milestone represents a GitHub milestone.
type Milestone struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// ListOpenMilestones fetches all open milestones for the repository.
func (c *GitHubClient) ListOpenMilestones() ([]Milestone, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/milestones?state=open&per_page=100", c.baseURL, c.owner, c.repo)
	body, status, err := c.doRequest("GET", reqURL, "")
	if err != nil {
		return nil, fmt.Errorf("listing milestones: %w", err)
	}

	if status != http.StatusOK {
		return nil, fmt.Errorf("listing milestones: status %d: %s", status, string(body))
	}

	var milestones []Milestone
	if err := json.Unmarshal(body, &milestones); err != nil {
		return nil, fmt.Errorf("decoding milestones: %w", err)
	}

	return milestones, nil
}

// SetMilestone sets the milestone on an issue or pull request.
func (c *GitHubClient) SetMilestone(number, milestoneNumber int) error {
	payload, err := json.Marshal(map[string]int{"milestone": milestoneNumber})
	if err != nil {
		return fmt.Errorf("encoding milestone for #%d: %w", number, err)
	}

	body, status, err := c.doRequest("PATCH", c.issueURL(number), string(payload))
	if err != nil {
		return fmt.Errorf("setting milestone on #%d: %w", number, err)
	}

	if status != http.StatusOK {
		return fmt.Errorf("setting milestone on #%d: status %d: %s", number, status, string(body))
	}

	return nil
}

// GetPullRequestFiles fetches the list of files changed in a pull request.
func (c *GitHubClient) GetPullRequestFiles(number int) ([]string, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/files?per_page=100", c.baseURL, c.owner, c.repo, number)
	body, status, err := c.doRequest("GET", reqURL, "")
	if err != nil {
		return nil, fmt.Errorf("fetching PR #%d files: %w", number, err)
	}

	if status != http.StatusOK {
		return nil, fmt.Errorf("fetching PR #%d files: status %d: %s", number, status, string(body))
	}

	var files []struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, fmt.Errorf("decoding PR #%d files: %w", number, err)
	}

	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Filename
	}

	return paths, nil
}

// GetPullRequestNodeID fetches just the node_id for a pull request (needed for GraphQL).
func (c *GitHubClient) GetPullRequestNodeID(number int) (string, error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, c.owner, c.repo, number)
	body, status, err := c.doRequest("GET", reqURL, "")
	if err != nil {
		return "", fmt.Errorf("fetching PR #%d: %w", number, err)
	}

	if status != http.StatusOK {
		return "", fmt.Errorf("fetching PR #%d: status %d: %s", number, status, string(body))
	}

	var pr struct {
		NodeID string `json:"node_id"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", fmt.Errorf("decoding PR #%d: %w", number, err)
	}

	return pr.NodeID, nil
}

// GetPullRequestStats fetches the additions and deletions for a pull request.
func (c *GitHubClient) GetPullRequestStats(number int) (additions, deletions int, err error) {
	reqURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, c.owner, c.repo, number)
	body, status, reqErr := c.doRequest("GET", reqURL, "")
	if reqErr != nil {
		return 0, 0, fmt.Errorf("fetching PR #%d stats: %w", number, reqErr)
	}

	if status != http.StatusOK {
		return 0, 0, fmt.Errorf("fetching PR #%d stats: status %d: %s", number, status, string(body))
	}

	var pr struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, 0, fmt.Errorf("decoding PR #%d stats: %w", number, err)
	}

	return pr.Additions, pr.Deletions, nil
}

// doGraphQL executes a GraphQL query against the GitHub API.
func (c *GitHubClient) doGraphQL(query string, variables map[string]any) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("encoding GraphQL request: %w", err)
	}

	respBody, status, err := c.doRequest("POST", c.baseURL+"/graphql", string(payload))
	if err != nil {
		return nil, fmt.Errorf("executing GraphQL request: %w", err)
	}

	if status != http.StatusOK {
		return nil, fmt.Errorf("GraphQL request: status %d: %s", status, string(respBody))
	}

	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("decoding GraphQL response: %w", err)
	}

	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL error: %s", resp.Errors[0].Message)
	}

	return resp.Data, nil
}

// AddToProjectBoard adds a PR to a GitHub Projects v2 board using GraphQL.
// It looks up the project by number, adds the item, then moves it to the
// specified status column (e.g., "Review").
func (c *GitHubClient) AddToProjectBoard(prNodeID string, projectNumber int, statusName string) error {
	// Step 1: Get project ID
	data, err := c.doGraphQL(`query($owner: String!, $number: Int!) {
		organization(login: $owner) { projectV2(number: $number) { id } }
	}`, map[string]any{"owner": c.owner, "number": projectNumber})
	if err != nil {
		// Try user-owned project as fallback
		data, err = c.doGraphQL(`query($owner: String!, $number: Int!) {
			user(login: $owner) { projectV2(number: $number) { id } }
		}`, map[string]any{"owner": c.owner, "number": projectNumber})
		if err != nil {
			return fmt.Errorf("looking up project #%d: %w", projectNumber, err)
		}
		var userResp struct {
			User struct {
				ProjectV2 struct {
					ID string `json:"id"`
				} `json:"projectV2"`
			} `json:"user"`
		}
		if err := json.Unmarshal(data, &userResp); err != nil {
			return fmt.Errorf("decoding user project: %w", err)
		}
		return c.addItemToProject(userResp.User.ProjectV2.ID, prNodeID, statusName)
	}

	var orgResp struct {
		Organization struct {
			ProjectV2 struct {
				ID string `json:"id"`
			} `json:"projectV2"`
		} `json:"organization"`
	}
	if err := json.Unmarshal(data, &orgResp); err != nil {
		return fmt.Errorf("decoding org project: %w", err)
	}

	return c.addItemToProject(orgResp.Organization.ProjectV2.ID, prNodeID, statusName)
}

func (c *GitHubClient) addItemToProject(projectID, contentID, statusName string) error {
	// Add item to project
	data, err := c.doGraphQL(`mutation($project: ID!, $content: ID!) {
		addProjectV2ItemById(input: {projectId: $project, contentId: $content}) {
			item { id }
		}
	}`, map[string]any{"project": projectID, "content": contentID})
	if err != nil {
		return fmt.Errorf("adding item to project: %w", err)
	}

	var addResp struct {
		AddProjectV2ItemByID struct {
			Item struct {
				ID string `json:"id"`
			} `json:"item"`
		} `json:"addProjectV2ItemById"`
	}
	if err := json.Unmarshal(data, &addResp); err != nil {
		return fmt.Errorf("decoding add-item response: %w", err)
	}
	itemID := addResp.AddProjectV2ItemByID.Item.ID

	// Find the Status field and the target option
	data, err = c.doGraphQL(`query($project: ID!) {
		node(id: $project) {
			... on ProjectV2 {
				field(name: "Status") {
					... on ProjectV2SingleSelectField {
						id
						options { id name }
					}
				}
			}
		}
	}`, map[string]any{"project": projectID})
	if err != nil {
		return fmt.Errorf("looking up Status field: %w", err)
	}

	var fieldResp struct {
		Node struct {
			Field struct {
				ID      string `json:"id"`
				Options []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"options"`
			} `json:"field"`
		} `json:"node"`
	}
	if err := json.Unmarshal(data, &fieldResp); err != nil {
		return fmt.Errorf("decoding Status field: %w", err)
	}

	var optionID string
	for _, opt := range fieldResp.Node.Field.Options {
		if strings.EqualFold(opt.Name, statusName) {
			optionID = opt.ID
			break
		}
	}
	if optionID == "" {
		return fmt.Errorf("status option %q not found in project", statusName)
	}

	// Set the status
	_, err = c.doGraphQL(`mutation($project: ID!, $item: ID!, $field: ID!, $value: String!) {
		updateProjectV2ItemFieldValue(input: {
			projectId: $project, itemId: $item, fieldId: $field,
			value: {singleSelectOptionId: $value}
		}) { projectV2Item { id } }
	}`, map[string]any{
		"project": projectID,
		"item":    itemID,
		"field":   fieldResp.Node.Field.ID,
		"value":   optionID,
	})
	if err != nil {
		return fmt.Errorf("setting status to %q: %w", statusName, err)
	}

	return nil
}
