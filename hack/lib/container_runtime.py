import os
import json

from .error import die
from .run import run

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
        if override not in ("docker", "podman"):
            die(f"CONTAINER_TOOL must be 'docker' or 'podman', got '{override}'")
        _container_runtime = override
        return _container_runtime

    # Prefer docker if its daemon is reachable. Some Docker CLI builds omit
    # Client.Platform.Name from `docker version --format json`, so checking
    # daemon availability directly (rather than parsing that field) avoids
    # misdetecting a working Docker install as podman when both are present.
    result = run("docker info", check=False, capture_output=True)
    if result.returncode == 0:
        _container_runtime = "docker"
        # Detect podman-docker wrapper: when podman is aliased as docker,
        # `docker info` succeeds but the output mentions "podman".
        if "emulate docker cli using podman." in (result.stdout or "").lower():
            _container_runtime = "podman"

        return _container_runtime

    # Fall back to podman
    result = run("podman version", check=False, capture_output=True)
    if result.returncode == 0:
        _container_runtime = "podman"
        return _container_runtime

    die("Neither docker nor podman is available")
    return ""  # unreachable, satisfies type checker

