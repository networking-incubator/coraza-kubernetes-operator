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
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
)

// -----------------------------------------------------------------------------
// Entry Point
// -----------------------------------------------------------------------------

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

// config holds all parsed CLI flags, environment overrides, and the command.
type config struct {
	verbose bool
	dryRun  bool
	owner   string
	repo    string
	issue   int
	project int
	command string
	token   string
}

// parseConfig parses CLI flags, applies environment variable fallbacks,
// and validates that all required fields are present.
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("github_project_manager", flag.ContinueOnError)

	var cfg config
	fs.BoolVar(&cfg.verbose, "verbose", false, "enable verbose output")
	fs.BoolVar(&cfg.verbose, "v", false, "enable verbose output (shorthand)")
	fs.BoolVar(&cfg.dryRun, "dry-run", false, "display changes without making them")
	fs.StringVar(&cfg.owner, "owner", "", "repository owner")
	fs.StringVar(&cfg.repo, "repo", "", "repository name")
	fs.IntVar(&cfg.issue, "issue", 0, "issue number")
	fs.IntVar(&cfg.project, "project", 0, "project board number (default: auto-discover oldest open)")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		return config{}, fmt.Errorf("missing command\n\n%s", usage())
	}
	cfg.command = remaining[0]

	if cfg.owner == "" {
		cfg.owner = os.Getenv("GITHUB_OWNER")
	}
	if cfg.repo == "" {
		cfg.repo = os.Getenv("GITHUB_REPO")
	}
	if cfg.issue == 0 {
		if v := os.Getenv("GITHUB_ISSUE"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return config{}, fmt.Errorf("invalid GITHUB_ISSUE %q: %w", v, err)
			}
			cfg.issue = n
		}
	}

	if cfg.owner == "" || cfg.repo == "" || cfg.issue == 0 {
		return config{}, fmt.Errorf("--owner, --repo, and --issue are required (or set GITHUB_OWNER, GITHUB_REPO, GITHUB_ISSUE)")
	}

	cfg.token = os.Getenv("GITHUB_TOKEN")
	if cfg.token == "" {
		return config{}, fmt.Errorf("GITHUB_TOKEN environment variable is required")
	}

	return cfg, nil
}

// newLogger returns a logging function that prints when verbose or dry-run
// mode is active.
func newLogger(cfg config) func(string, ...any) {
	return func(format string, a ...any) {
		if cfg.verbose || cfg.dryRun {
			fmt.Printf(format+"\n", a...)
		}
	}
}

// -----------------------------------------------------------------------------
// Commands
// -----------------------------------------------------------------------------

// run is the top-level entry point: parses config, creates the client,
// and dispatches to the appropriate command.
func run(args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}

	client := NewGitHubClient(cfg.token, cfg.owner, cfg.repo)
	return dispatch(cfg, client)
}

// dispatch fetches the issue and routes to the appropriate command handler.
func dispatch(cfg config, client *GitHubClient) error {
	log := newLogger(cfg)

	log("Fetching issue #%d from %s/%s", cfg.issue, cfg.owner, cfg.repo)
	iss, err := client.GetIssue(cfg.issue)
	if err != nil {
		return err
	}

	log("Issue #%d: state=%s milestone=%v labels=%v", iss.Number, iss.State, iss.HasMilestone(), iss.Labels)

	switch cfg.command {
	case "update-labels":
		return runUpdateLabels(client, cfg.issue, iss.Labels, iss.HasMilestone(), iss.Body, cfg.dryRun, log)
	case "close-declined":
		return runCloseDeclined(client, cfg.issue, iss.Labels, iss.HasMilestone(), iss.State, cfg.dryRun, log)
	case "triage-pr":
		return runTriagePR(client, cfg.issue, iss, cfg.project, cfg.dryRun, os.Stderr, log)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cfg.command, usage())
	}
}

// -----------------------------------------------------------------------------
// Issue commands
// -----------------------------------------------------------------------------

func runUpdateLabels(client *GitHubClient, number int, labels []string, hasMilestone bool, body string, dryRun bool, log func(string, ...any)) error {
	if slices.Contains(labels, "triage/declined") {
		log("Issue is declined, skipping label updates")
		return nil
	}

	result := computeLabelUpdates(labels, hasMilestone)
	effective := effectiveLabels(labels, result)

	result.LabelsToAdd = append(result.LabelsToAdd, computeSizeLabels(effective)...)
	result.LabelsToAdd = append(result.LabelsToAdd, computeAreaLabels(effective, body)...)

	if len(result.LabelsToAdd) == 0 && len(result.LabelsToRemove) == 0 {
		log("No label changes needed")
		return nil
	}

	if err := applyLabels(client, number, result.LabelsToAdd, result.LabelsToRemove, dryRun, log); err != nil {
		return err
	}

	if dryRun {
		fmt.Println("dry-run: no changes applied")
	}

	return nil
}

func runCloseDeclined(client *GitHubClient, number int, labels []string, hasMilestone bool, state string, dryRun bool, log func(string, ...any)) error {
	result := computeDeclined(labels, hasMilestone, state)

	if result == nil {
		log("Issue is not declined, nothing to do")
		return nil
	}

	for _, l := range result.LabelsToRemove {
		log("Removing label: %s", l)
	}
	if result.RemoveMilestone {
		log("Removing milestone")
	}
	if result.CloseIssue {
		log("Closing issue")
	}

	if dryRun {
		fmt.Println("dry-run: no changes applied")
		return nil
	}

	for _, l := range result.LabelsToRemove {
		if err := client.RemoveLabel(number, l); err != nil {
			return err
		}
	}

	if result.RemoveMilestone {
		if err := client.RemoveMilestone(number); err != nil {
			return err
		}
	}

	if result.CloseIssue {
		if err := client.CloseIssue(number); err != nil {
			return err
		}
	}

	return nil
}

// -----------------------------------------------------------------------------
// PR commands
// -----------------------------------------------------------------------------

func runTriagePR(client *GitHubClient, number int, iss *Issue, projectNumber int, dryRun bool, warnWriter io.Writer, log func(string, ...any)) error {
	prInfo, err := client.GetPullRequestInfo(number)
	if err != nil {
		return err
	}

	files, err := client.GetPullRequestFiles(number)
	if err != nil {
		return err
	}
	log("PR #%d changed %d files", number, len(files))

	var labelsToAdd []string
	labelsToAdd = append(labelsToAdd, computePRAreaLabels(iss.Labels, files)...)
	labelsToAdd = append(labelsToAdd, computePRSizeLabel(iss.Labels, prInfo.Additions, prInfo.Deletions)...)

	if err := applyLabels(client, number, labelsToAdd, nil, dryRun, log); err != nil {
		return err
	}

	if !iss.HasMilestone() {
		if err := assignMilestone(client, number, dryRun, log); err != nil {
			return err
		}
	} else {
		log("PR already has a milestone, skipping")
	}

	if !dryRun {
		projectID, projectErr := resolveProjectID(client, projectNumber, log)
		if projectErr != nil {
			fmt.Fprintf(warnWriter, "::warning::could not resolve project board: %v\n", projectErr)
		} else {
			if err := client.addItemToProject(projectID, prInfo.NodeID, "Review"); err != nil {
				fmt.Fprintf(warnWriter, "::warning::could not add to project board: %v\n", err)
			}
		}
	}

	if dryRun {
		fmt.Println("dry-run: no changes applied")
	}

	return nil
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// applyLabels logs and applies label additions and removals. In dry-run mode,
// labels are logged but no API calls are made.
func applyLabels(client *GitHubClient, number int, toAdd, toRemove []string, dryRun bool, log func(string, ...any)) error {
	for _, l := range toAdd {
		log("Adding label: %s", l)
	}
	for _, l := range toRemove {
		log("Removing label: %s", l)
	}

	if dryRun {
		return nil
	}

	if len(toAdd) > 0 {
		if err := client.AddLabels(number, toAdd); err != nil {
			return err
		}
	}

	for _, l := range toRemove {
		if err := client.RemoveLabel(number, l); err != nil {
			return err
		}
	}

	return nil
}

// resolveProjectID returns the project node ID, either by looking up an
// explicit project number or by auto-discovering the oldest open project.
func resolveProjectID(client *GitHubClient, projectNumber int, log func(string, ...any)) (string, error) {
	if projectNumber > 0 {
		log("Looking up project board #%d", projectNumber)
		return client.lookupProjectID(projectNumber)
	}
	log("Auto-discovering oldest open project board")
	return client.findOldestOpenProject()
}

// assignMilestone finds the lowest semver milestone and assigns it to the
// given issue/PR. Logs a skip message if no valid milestone exists.
func assignMilestone(client *GitHubClient, number int, dryRun bool, log func(string, ...any)) error {
	milestones, err := client.ListOpenMilestones()
	if err != nil {
		return err
	}

	m, err := findLowestMilestone(milestones)
	if err != nil {
		log("Skipping milestone: %v", err)
		return nil
	}

	log("Setting milestone: %s (#%d)", m.Title, m.Number)
	if !dryRun {
		if err := client.SetMilestone(number, m.Number); err != nil {
			return err
		}
	}

	return nil
}

// -----------------------------------------------------------------------------
// Usage
// -----------------------------------------------------------------------------

func usage() string {
	return `Usage: github_project_manager [flags] <command>

Issue Commands:
  update-labels     Apply triage label rules based on milestone status
  close-declined    Handle declined issues (close, remove labels/milestone)

PR Commands:
  triage-pr         Apply area labels, milestone, size labels, and add to project board

Flags:
  -v, --verbose     Enable verbose output
  --dry-run         Display changes without making them
  --owner           Repository owner (or GITHUB_OWNER env)
  --repo            Repository name (or GITHUB_REPO env)
  --issue           Issue/PR number (or GITHUB_ISSUE env)
  --project         Project number for board management (default: auto-discover oldest open)

Environment:
  GITHUB_TOKEN      GitHub API token (required)`
}
