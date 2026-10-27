"""Shared utilities for hack/ scripts."""

import os
import subprocess
import sys

import yaml

# ---------------------------------------------------------------------------
# YAML: semver scalars for OLM CSV
# ---------------------------------------------------------------------------
#
# Unquoted "0.0.0" can round-trip through YAML→JSON as a number; CSV
# spec.version uses OperatorVersion, whose UnmarshalJSON only accepts a JSON
# string. Force double-quoted scalars so validation always sees a semver string.


class SemverYAML(str):
    """Semver string that PyYAML emits quoted (required for OLM CSV spec.version)."""


def _represent_semver_yaml(dumper, data):
    return dumper.represent_scalar("tag:yaml.org,2002:str", str(data), style='"')


yaml.add_representer(SemverYAML, _represent_semver_yaml)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

HELM_CHART_DIR = "charts/coraza-kubernetes-operator"
HELM_RELEASE_NAME = "coraza-kubernetes-operator"

# ---------------------------------------------------------------------------
# Error Handling
# ---------------------------------------------------------------------------


def die(msg: str, code: int = 1):
    """Print an error message to stderr and exit."""
    print(f"ERROR: {msg}", file=sys.stderr)
    sys.exit(code)


# ---------------------------------------------------------------------------
# Shell Execution
# ---------------------------------------------------------------------------


def run(cmd: str, *, check: bool = True, capture_output: bool = False,
        input_str: str = None, verbose: bool = False) -> subprocess.CompletedProcess:
    """Run a shell command. Returns CompletedProcess; raises on failure when check=True."""
    if verbose:
        print(f"+ {cmd}")
    return subprocess.run(
        cmd, shell=True, check=check, text=True,
        input=input_str, capture_output=capture_output,
    )


# ---------------------------------------------------------------------------
# Container Runtime Detection
# ---------------------------------------------------------------------------

_container_runtime: str = ""


def detect_container_runtime() -> str:
    """Detect whether docker or podman is available. Caches the result."""
    global _container_runtime
    if _container_runtime:
        return _container_runtime

    # Honor an explicit CONTAINER_TOOL override (matches the Makefile variable).
    # This lets callers force a runtime instead of relying on autodetection.
    override = os.environ.get("CONTAINER_TOOL", "").strip()
    if override:
        _container_runtime = override
        return _container_runtime

    # Prefer docker if its daemon is reachable. Some Docker CLI builds omit
    # Client.Platform.Name from `docker version --format json`, so checking
    # daemon availability directly (rather than parsing that field) avoids
    # misdetecting a working Docker install as podman when both are present.
    result = run("docker info", check=False, capture_output=True)
    if result.returncode == 0:
        _container_runtime = "docker"
        return _container_runtime

    # Fall back to podman
    result = run("podman version", check=False, capture_output=True)
    if result.returncode == 0:
        _container_runtime = "podman"
        return _container_runtime

    die("Neither docker nor podman is available")
    return ""  # unreachable, satisfies type checker


# ---------------------------------------------------------------------------
# YAML Helpers
# ---------------------------------------------------------------------------


def load_yaml_docs(path: str) -> list:
    """Load a multi-document YAML file, filtering out empty documents."""
    with open(path) as f:
        return [d for d in yaml.safe_load_all(f) if d is not None]


def write_yaml_docs(path: str, docs: list):
    """Write multiple YAML documents to a file with explicit document markers."""
    with open(path, "w") as f:
        yaml.dump_all(docs, f, default_flow_style=False, sort_keys=False,
                      explicit_start=True)


def write_yaml(path: str, data: dict):
    """Write a single YAML document to a file."""
    with open(path, "w") as f:
        yaml.dump(data, f, default_flow_style=False, sort_keys=False, width=1000)
