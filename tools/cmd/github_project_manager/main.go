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
	"os"
	"slices"
	"strconv"
)

// -----------------------------------------------------------------------------
// Entry point
// -----------------------------------------------------------------------------

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("github_project_manager", flag.ContinueOnError)

	var (
		verbose bool
		dryRun  bool
		owner   string
		repo    string
		issue   int
		project int
	)

	fs.BoolVar(&verbose, "verbose", false, "enable verbose output")
	fs.BoolVar(&verbose, "v", false, "enable verbose output (shorthand)")
	fs.BoolVar(&dryRun, "dry-run", false, "display changes without making them")
	fs.StringVar(&owner, "owner", "", "repository owner")
	fs.StringVar(&repo, "repo", "", "repository name")
	fs.IntVar(&issue, "issue", 0, "issue number")
	fs.IntVar(&project, "project", 1, "project board number")

	if err := fs.Parse(args); err != nil {
		return err
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		return fmt.Errorf("missing command\n\n%s", usage())
	}

	command := remaining[0]

	if owner == "" {
		owner = os.Getenv("GITHUB_OWNER")
	}
	if repo == "" {
		repo = os.Getenv("GITHUB_REPO")
	}
	if issue == 0 {
		if v := os.Getenv("GITHUB_ISSUE"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("invalid GITHUB_ISSUE %q: %w", v, err)
			}
			issue = n
		}
	}

	if owner == "" || repo == "" || issue == 0 {
		return fmt.Errorf("--owner, --repo, and --issue are required (or set GITHUB_OWNER, GITHUB_REPO, GITHUB_ISSUE)")
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return fmt.Errorf("GITHUB_TOKEN environment variable is required")
	}

	log := func(format string, a ...any) {
		if verbose || dryRun {
			fmt.Printf(format+"\n", a...)
		}
	}

	client := NewGitHubClient(token, owner, repo)

	log("Fetching issue #%d from %s/%s", issue, owner, repo)
	iss, err := client.GetIssue(issue)
	if err != nil {
		return err
	}

	log("Issue #%d: state=%s milestone=%v labels=%v", iss.Number, iss.State, iss.HasMilestone(), iss.Labels)

	switch command {
	case "update-labels":
		return runUpdateLabels(client, issue, iss.Labels, iss.HasMilestone(), iss.Body, dryRun, log)

	case "close-declined":
		return runCloseDeclined(client, issue, iss.Labels, iss.HasMilestone(), iss.State, dryRun, log)

	case "triage-pr":
		return runTriagePR(client, issue, iss, project, dryRun, log)

	default:
		return fmt.Errorf("unknown command %q\n\n%s", command, usage())
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

	for _, l := range result.LabelsToAdd {
		log("Adding label: %s", l)
	}
	for _, l := range result.LabelsToRemove {
		log("Removing label: %s", l)
	}

	if dryRun {
		fmt.Println("dry-run: no changes applied")
		return nil
	}

	if len(result.LabelsToAdd) > 0 {
		if err := client.AddLabels(number, result.LabelsToAdd); err != nil {
			return err
		}
	}

	for _, l := range result.LabelsToRemove {
		if err := client.RemoveLabel(number, l); err != nil {
			return err
		}
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

func runTriagePR(client *GitHubClient, number int, iss *Issue, projectNumber int, dryRun bool, log func(string, ...any)) error {
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

	if len(labelsToAdd) > 0 {
		for _, l := range labelsToAdd {
			log("Adding label: %s", l)
		}
		if !dryRun {
			if err := client.AddLabels(number, labelsToAdd); err != nil {
				return err
			}
		}
	}

	if !iss.HasMilestone() {
		milestones, err := client.ListOpenMilestones()
		if err != nil {
			return err
		}
		m, err := findLowestMilestone(milestones)
		if err != nil {
			log("Skipping milestone: %v", err)
		} else {
			log("Setting milestone: %s (#%d)", m.Title, m.Number)
			if !dryRun {
				if err := client.SetMilestone(number, m.Number); err != nil {
					return err
				}
			}
		}
	} else {
		log("PR already has a milestone, skipping")
	}

	log("Adding PR to project board #%d under Review", projectNumber)
	if !dryRun {
		if err := client.AddToProjectBoard(prInfo.NodeID, projectNumber, "Review"); err != nil {
			log("Warning: could not add to project board: %v", err)
		}
	}

	if dryRun {
		fmt.Println("dry-run: no changes applied")
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
  --project         Project number for board management (default: 1)

Environment:
  GITHUB_TOKEN      GitHub API token (required)`
}
