/*
Copyright 2026 Shane Utt.

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
	userAgent      = "github_issue_manager/1.0"
)

// Issue represents a GitHub issue with the fields we care about.
type Issue struct {
	Number    int       `json:"number"`
	State     string    `json:"state"`
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
