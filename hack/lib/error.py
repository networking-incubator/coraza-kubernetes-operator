import sys

# ---------------------------------------------------------------------------
# Error Handling
# ---------------------------------------------------------------------------


def die(msg: str, code: int = 1):
    """Print an error message to stderr and exit."""
    print(f"ERROR: {msg}", file=sys.stderr)
    sys.exit(code)
