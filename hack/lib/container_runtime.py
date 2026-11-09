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

    # Try docker first
    result = run("docker version --format json", check=False, capture_output=True)
    if result.returncode == 0:
        try:
            info = json.loads(result.stdout)
            platform = info.get("Client", {}).get("Platform", {}).get("Name", "")
            if "Docker Engine" in platform:
                _container_runtime = "docker"
                return _container_runtime
        except (json.JSONDecodeError, KeyError, AttributeError):
            pass

    # Fall back to podman
    result = run("podman version", check=False, capture_output=True)
    if result.returncode == 0:
        _container_runtime = "podman"
        return _container_runtime

    die("Neither docker nor podman is available")
    return ""  # unreachable, satisfies type checker
