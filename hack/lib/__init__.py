"""Shared utilities for hack/ scripts.

Import from the submodules directly, e.g. ``from lib.error import die``.
This package deliberately re-exports nothing: anything imported here would be
loaded by every caller, and the Makefile resolves CONTAINER_TOOL through
``lib.container_runtime`` on every invocation without PyYAML installed.
"""
