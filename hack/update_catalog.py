#!/usr/bin/env python3
"""
Update the OLM file-based catalog with a new bundle version.

Appends an olm.bundle entry and updates the olm.channel entries list,
setting `replaces` to the previous latest entry so OLM knows the upgrade path.
"""

import argparse
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


def find_doc(docs: list, schema: str, **match) -> tuple:
    """Return (index, doc) for the first document matching schema and fields."""
    for i, doc in enumerate(docs):
        if doc.get("schema") != schema:
            continue
        if all(doc.get(k) == v for k, v in match.items()):
            return i, doc
    return -1, None


def main():
    parser = argparse.ArgumentParser(
        description="Update OLM file-based catalog with a new bundle version")
    parser.add_argument("--catalog-file", required=True,
                        help="Path to catalog.yaml")
    parser.add_argument("--bundle-image", required=True,
                        help="Full bundle image reference (repo:tag)")
    parser.add_argument("--version", required=True,
                        help="Operator version (with or without 'v' prefix)")
    parser.add_argument("--channel", default="alpha",
                        help="OLM channel name (default: alpha)")
    parser.add_argument("--package-name",
                        default="coraza-kubernetes-operator",
                        help="OLM package name")
    args = parser.parse_args()

    version = args.version.lstrip("v")
    entry_name = f"{args.package_name}.v{version}"

    docs = load_catalog(args.catalog_file)

    _, existing = find_doc(docs, "olm.bundle", name=entry_name)
    if existing:
        print(f"Bundle {entry_name} already exists, nothing to do",
              file=sys.stderr)
        return

    chan_idx, channel_doc = find_doc(docs, "olm.channel", name=args.channel)
    if not channel_doc:
        print(f"ERROR: channel '{args.channel}' not found in {args.catalog_file}",
              file=sys.stderr)
        sys.exit(1)

    entries = channel_doc.get("entries", [])
    previous = entries[-1]["name"] if entries else None

    new_bundle = {
        "schema": "olm.bundle",
        "package": args.package_name,
        "name": entry_name,
        "image": args.bundle_image,
    }
    docs.insert(chan_idx, new_bundle)

    new_entry = {"name": entry_name}
    if previous:
        new_entry["replaces"] = previous
    entries.append(new_entry)

    write_catalog(args.catalog_file, docs)
    replaces_msg = f" (replaces {previous})" if previous else ""
    print(f"Added {entry_name} to catalog{replaces_msg}", file=sys.stderr)


if __name__ == "__main__":
    main()
