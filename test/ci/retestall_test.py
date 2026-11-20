"""Behavior check for the PR comment retry workflow."""

import json
import os
from pathlib import Path
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[2]
script = ROOT / ".github/scripts/retest.sh"
workflow = (ROOT / ".github/workflows/retest.yml").read_text()
newline = json.loads(r'"\n"')
for command, expected_count in (("retest", 1), ("retestall", 2)):
    expression = (
        "contains(format('{0}{1}{0}', fromJSON('\"\\n\"'), github.event.comment.body), "
        f"format('{{0}}/{command}{{0}}', fromJSON('\"\\n\"')))"
    )
    assert workflow.count(expression) == expected_count
    for body in (f"/{command}", f"/{command}\nthanks", f"please\n/{command}", f"before\n/{command}\nafter"):
        assert f"{newline}/{command}{newline}" in f"{newline}{body}{newline}"
    for body in (f"/{command}-extra", f"please /{command}", f"/{command} suffix"):
        assert f"{newline}/{command}{newline}" not in f"{newline}{body}{newline}"


def run_case(runs, list_error=False, retest_all=True, total_count=3):
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        (directory / "gh").write_text(
            """#!/usr/bin/env python3
import json, os, subprocess, sys
from pathlib import Path
args = sys.argv[1:]
if args[:2] == ['api', 'repos/org/repo/pulls/42']:
    print('abc123')
elif args[0] == 'api' and 'actions/runs?' in args[1]:
    assert 'head_sha=abc123&per_page=1' in args[1]
    print(os.environ['MOCK_TOTAL_COUNT'])
elif args[:2] == ['api', '--paginate']:
    if os.environ['MOCK_LIST_ERROR'] == 'true':
        sys.exit('list failed')
    assert 'head_sha=abc123&per_page=100' in args[2]
    result = subprocess.run(
        ['jq', '-r', args[4]],
        input=os.environ['MOCK_RUNS'], text=True, capture_output=True)
    if result.returncode:
        sys.exit(result.stderr)
    print(result.stdout, end='')
elif args[:2] == ['run', 'rerun']:
    with Path(os.environ['MOCK_LOG']).open('a') as log:
        print(' '.join(args), file=log)
elif args[:2] == ['run', 'list']:
    print(chr(9).join(['4', 'build', 'https://example.test/4']))
else:
    sys.exit('unexpected gh call: ' + repr(args))
"""
        )
        (directory / "gh").chmod(0o755)
        env = dict(
            os.environ,
            PATH=f"{tmp}:{os.environ['PATH']}",
            GH_REPO="org/repo",
            PR_NUMBER="42",
            RETEST_ALL=str(retest_all).lower(),
            MOCK_RUNS=json.dumps({"workflow_runs": runs}),
            MOCK_LIST_ERROR=str(list_error).lower(),
            MOCK_TOTAL_COUNT=str(total_count),
            MOCK_LOG=str(directory / "log"),
            GITHUB_STEP_SUMMARY=str(directory / "summary"),
        )
        result = subprocess.run(["bash", script], text=True, capture_output=True, env=env)
        assert (result.returncode != 0) == (list_error or total_count >= 1000), result.stderr
        summary = (directory / "summary").read_text()
        reruns = (directory / "log").read_text() if (directory / "log").exists() else ""
        return summary, reruns


def workflow_run(number, status, pr=42):
    return {
        "id": number,
        "name": f"run-{number}",
        "html_url": f"https://example.test/{number}",
        "status": status,
        "pull_requests": [{"number": pr}],
    }


summary, reruns = run_case(
    [
        workflow_run(1, "completed"),
        workflow_run(2, "in_progress"),
        workflow_run(3, "completed", pr=43),
    ]
)
assert reruns == "run rerun 1 --repo org/repo\n", reruns
assert "Retried: [run-1]" in summary
assert "Active: [run-2]" in summary
assert "run-3" not in summary

summary, reruns = run_case([workflow_run(2, "in_progress")])
assert not reruns
assert "No completed runs for this PR and commit" in summary

summary, reruns = run_case([], list_error=True)
assert not reruns
assert "Failed to list workflow runs" in summary

summary, reruns = run_case([workflow_run(1, "completed")], total_count=1000)
assert not reruns
assert "1,000" in summary

summary, reruns = run_case([workflow_run(1, "completed")], total_count=999)
assert reruns == "run rerun 1 --repo org/repo\n", reruns

summary, reruns = run_case([], retest_all=False)
assert reruns == "run rerun 4 --failed --repo org/repo\n", reruns

print("retestall behavior passed")
