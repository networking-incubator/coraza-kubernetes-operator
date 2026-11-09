#!/usr/bin/env python3
"""
Add a new version to the OLM file-based catalog channel entries.

Sets `replaces` to the previous latest entry for OLM upgrade path.
Idempotent: skips if the version already exists.

Usage: update_catalog.py <catalog-file> <version> [package-name] [channel]
"""

import sys

from lib.error import die
from lib.yaml_helpers import load_yaml_docs, write_yaml_docs

# ---------------------------------------------------------------------------
# Catalog Helpers
# ---------------------------------------------------------------------------


def find_channel(docs: list, channel: str, package_name: str) -> dict:
    """Return the olm.channel document matching the given channel and package."""
    for doc in docs:
        if (doc.get("schema") == "olm.channel"
                and doc.get("name") == channel
                and doc.get("package") == package_name):
            return doc
    return None


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main():
    if len(sys.argv) < 3:
        die(f"usage: {sys.argv[0]} <catalog-file> <version> [package-name] [channel]")

    catalog_file = sys.argv[1]
    version = sys.argv[2].lstrip("v")
    package_name = sys.argv[3] if len(sys.argv) > 3 else "coraza-kubernetes-operator"
    channel = sys.argv[4] if len(sys.argv) > 4 else "alpha"

    entry_name = f"{package_name}.v{version}"

    docs = load_yaml_docs(catalog_file)
    channel_doc = find_channel(docs, channel, package_name)
    if not channel_doc:
        die(f"channel '{channel}' not found")

    entries = channel_doc.setdefault("entries", [])
    for e in entries:
        if e["name"] == entry_name:
            print(f"Entry {entry_name} already exists, nothing to do", file=sys.stderr)
            return

    # Set replaces to the current latest entry for upgrade path
    previous = entries[-1]["name"] if entries else None
    new_entry = {"name": entry_name}
    if previous:
        new_entry["replaces"] = previous
    entries.append(new_entry)

    write_yaml_docs(catalog_file, docs)
    replaces_msg = f" (replaces {previous})" if previous else ""
    print(f"Added {entry_name} to catalog{replaces_msg}", file=sys.stderr)


if __name__ == "__main__":
    main()
