import subprocess

# ---------------------------------------------------------------------------
# Shell Execution
# ---------------------------------------------------------------------------


def run(
    cmd: str,
    *,
    check: bool = True,
    capture_output: bool = False,
    input_str: str = None,
    verbose: bool = False,
) -> subprocess.CompletedProcess:
    """Run a shell command. Returns CompletedProcess; raises on failure when check=True."""
    if verbose:
        print(f"+ {cmd}")
    return subprocess.run(
        cmd,
        shell=True,
        check=check,
        text=True,
        input=input_str,
        capture_output=capture_output,
    )
