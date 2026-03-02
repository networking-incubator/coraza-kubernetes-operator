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
	"flag"
	"fmt"
	"os"
	"strconv"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("github_issue_manager", flag.ContinueOnError)

	var (
		verbose bool
		dryRun  bool
		owner   string
		repo    string
		issue   int
	)

	fs.BoolVar(&verbose, "verbose", false, "enable verbose output")
	fs.BoolVar(&verbose, "v", false, "enable verbose output (shorthand)")
	fs.BoolVar(&dryRun, "dry-run", false, "display changes without making them")
	fs.StringVar(&owner, "owner", "", "repository owner")
	fs.StringVar(&repo, "repo", "", "repository name")
	fs.IntVar(&issue, "issue", 0, "issue number")

	if err := fs.Parse(args); err != nil {
		return err
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		return fmt.Errorf("missing command: expected 'update-labels' or 'close-declined'\n\n%s", usage())
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
		return runUpdateLabels(client, issue, iss.Labels, iss.HasMilestone(), dryRun, log)

	case "close-declined":
		return runCloseDeclined(client, issue, iss.Labels, iss.HasMilestone(), iss.State, dryRun, log)

	default:
		return fmt.Errorf("unknown command %q: expected 'update-labels' or 'close-declined'\n\n%s", command, usage())
	}
}

func runUpdateLabels(client *GitHubClient, number int, labels []string, hasMilestone, dryRun bool, log func(string, ...any)) error {
	// Skip declined issues — they are handled entirely by close-declined.
	if contains(labels, "triage/declined") {
		log("Issue is declined, skipping label updates")
		return nil
	}

	result := ComputeLabelUpdates(labels, hasMilestone)

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
	result := ComputeDeclined(labels, hasMilestone, state)

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

func usage() string {
	return `Usage: github_issue_manager [flags] <command>

Commands:
  update-labels     Apply triage label rules based on milestone status
  close-declined    Handle declined issues (close, remove labels/milestone)

Flags:
  -v, --verbose     Enable verbose output
  --dry-run         Display changes without making them
  --owner           Repository owner (or GITHUB_OWNER env)
  --repo            Repository name (or GITHUB_REPO env)
  --issue           Issue number (or GITHUB_ISSUE env)

Environment:
  GITHUB_TOKEN      GitHub API token (required)`
}
