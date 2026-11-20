#!/usr/bin/env bash
set -euo pipefail

HEAD_SHA=$(gh api "repos/${GH_REPO}/pulls/${PR_NUMBER}" --jq '.head.sha')
if [ -z "${HEAD_SHA}" ] || [ "${HEAD_SHA}" = "null" ]; then
  echo "Failed to resolve PR head SHA for #${PR_NUMBER}"
  exit 1
fi

if [ "${RETEST_ALL}" = "true" ]; then
  {
    echo "## Retried all jobs in completed PR runs"
    echo
    echo "PR #${PR_NUMBER} head \`${HEAD_SHA}\`"
    echo
  } >> "${GITHUB_STEP_SUMMARY}"

  if ! total=$(gh api "repos/${GH_REPO}/actions/runs?head_sha=${HEAD_SHA}&per_page=1" --jq '.total_count'); then
    echo "Failed to count workflow runs for this PR and commit." >> "${GITHUB_STEP_SUMMARY}"
    exit 1
  fi
  if ! [[ "${total}" =~ ^[0-9]+$ ]]; then
    echo "Invalid workflow run count returned by GitHub." >> "${GITHUB_STEP_SUMMARY}"
    exit 1
  fi
  if (( total >= 1000 )); then
    echo "GitHub returned at least 1,000 runs for this commit; the search may be truncated. No runs were retried." >> "${GITHUB_STEP_SUMMARY}"
    exit 1
  fi

  if ! runs=$(gh api --paginate \
    "repos/${GH_REPO}/actions/runs?head_sha=${HEAD_SHA}&per_page=100" \
    --jq ".workflow_runs[] | select(any(.pull_requests[]?; .number == ${PR_NUMBER})) | [.id, .name, .html_url, .status] | @tsv"); then
    echo "Failed to list workflow runs for this PR and commit." >> "${GITHUB_STEP_SUMMARY}"
    exit 1
  fi

  eligible=0
  errors=0
  if [ -n "${runs}" ]; then
    while IFS=$'\t' read -r id name url status; do
      if [ "${status}" != "completed" ]; then
        echo "- Active: [${name}](${url}) (\`${id}\`, ${status})" >> "${GITHUB_STEP_SUMMARY}"
        continue
      fi
      eligible=$((eligible + 1))
      if gh run rerun "${id}" --repo "${GH_REPO}"; then
        echo "- Retried: [${name}](${url}) (\`${id}\`)" >> "${GITHUB_STEP_SUMMARY}"
      else
        echo "- Failed to retry: [${name}](${url}) (\`${id}\`)" >> "${GITHUB_STEP_SUMMARY}"
        errors=1
      fi
    done <<< "${runs}"
  fi

  if [ "${eligible}" -eq 0 ]; then
    echo "No completed runs for this PR and commit were eligible for retry." >> "${GITHUB_STEP_SUMMARY}"
  fi
  exit "${errors}"
fi

mapfile -t RUNS < <(
  gh run list --repo "${GH_REPO}" --commit "${HEAD_SHA}" \
    --json databaseId,name,conclusion,url \
    --jq '.[] | select(.conclusion == "failure" or .conclusion == "cancelled") | [.databaseId, .name, .url] | @tsv'
)

if [ "${#RUNS[@]}" -eq 0 ]; then
  echo "No failed or cancelled workflow runs found for SHA ${HEAD_SHA}"
  exit 1
fi

{
  echo "## Retried workflow runs"
  echo
  echo "PR #${PR_NUMBER} head \`${HEAD_SHA}\`"
  echo
} >> "${GITHUB_STEP_SUMMARY}"

for row in "${RUNS[@]}"; do
  id="${row%%$'\t'*}"
  rest="${row#*$'\t'}"
  name="${rest%%$'\t'*}"
  url="${rest#*$'\t'}"

  echo "Re-running failed jobs for: ${name} (${id})"
  gh run rerun "${id}" --failed --repo "${GH_REPO}"
  echo "- [${name}](${url}) (\`${id}\`)" >> "${GITHUB_STEP_SUMMARY}"
done
