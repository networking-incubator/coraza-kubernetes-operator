#!/usr/bin/env python3
"""
List bundle image refs from an OLM file-based catalog channel.

Reads all olm.channel entries from the catalog file and prints the
corresponding bundle image references (space-separated).

Usage: list_bundle_images.py <catalog-file> <bundle-image-base> [package-name]
"""

import sys

import yaml


def main():
    if len(sys.argv) < 3:
        print(f"usage: {sys.argv[0]} <catalog-file> <bundle-image-base> [package-name]",
              file=sys.stderr)
        sys.exit(1)

    catalog_file = sys.argv[1]
    bundle_img_base = sys.argv[2]
    package_name = sys.argv[3] if len(sys.argv) > 3 else "coraza-kubernetes-operator"

    with open(catalog_file) as f:
        docs = [d for d in yaml.safe_load_all(f) if d]

    images = []
    for doc in docs:
        if (doc.get("schema") == "olm.channel"
                and doc.get("package") == package_name):
            for entry in doc.get("entries", []):
                version = entry["name"].split(".v", 1)[1]
                images.append(f"{bundle_img_base}:v{version}")

    print(" ".join(images))


if __name__ == "__main__":
    main()
