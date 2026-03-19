#!/usr/bin/env python3
"""
Add a new version to the OLM file-based catalog channel entries.

Sets `replaces` to the previous latest entry for OLM upgrade path.
Idempotent: skips if the version already exists.

Usage: update_catalog.py <catalog-file> <version> [package-name] [channel]
"""

import sys

import yaml


def load_catalog(path: str) -> list:
    with open(path) as f:
        docs = list(yaml.safe_load_all(f))
    return [d for d in docs if d is not None]


def write_catalog(path: str, docs: list):
    with open(path, "w") as f:
        yaml.dump_all(docs, f, default_flow_style=False, sort_keys=False,
                      explicit_start=True)


def find_channel(docs: list, channel: str, package_name: str) -> dict:
    for doc in docs:
        if (doc.get("schema") == "olm.channel"
                and doc.get("name") == channel
                and doc.get("package") == package_name):
            return doc
    return None


def main():
    if len(sys.argv) < 3:
        print(f"usage: {sys.argv[0]} <catalog-file> <version> [package-name]",
              file=sys.stderr)
        sys.exit(1)

    catalog_file = sys.argv[1]
    version = sys.argv[2].lstrip("v")
    package_name = sys.argv[3] if len(sys.argv) > 3 else "coraza-kubernetes-operator"
    channel = sys.argv[4] if len(sys.argv) > 4 else "alpha"

    entry_name = f"{package_name}.v{version}"

    docs = load_catalog(catalog_file)
    channel_doc = find_channel(docs, channel, package_name)
    if not channel_doc:
        print(f"ERROR: channel '{channel}' not found", file=sys.stderr)
        sys.exit(1)

    entries = channel_doc.setdefault("entries", [])
    for e in entries:
        if e["name"] == entry_name:
            print(f"Entry {entry_name} already exists, nothing to do",
                  file=sys.stderr)
            return

    previous = entries[-1]["name"] if entries else None
    new_entry = {"name": entry_name}
    if previous:
        new_entry["replaces"] = previous
    entries.append(new_entry)

    write_catalog(catalog_file, docs)
    replaces_msg = f" (replaces {previous})" if previous else ""
    print(f"Added {entry_name} to catalog{replaces_msg}", file=sys.stderr)


if __name__ == "__main__":
    main()
