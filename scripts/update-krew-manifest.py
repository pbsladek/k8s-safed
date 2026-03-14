#!/usr/bin/env python3
"""
update-krew-manifest.py

Reads the GoReleaser-produced checksums.txt and rewrites plugin.yaml with
the correct download URLs and SHA256 checksums for the given release version.

Usage:
    VERSION=v0.1.0 python3 scripts/update-krew-manifest.py

Environment variables:
    VERSION   - the release tag, e.g. v0.1.0  (required)
    REPO      - GitHub repository slug         (default: pbsladek/k8s-safed)
    CHECKSUMS - path to checksums file         (default: dist/checksums.txt)
    MANIFEST  - path to krew plugin manifest   (default: plugin.yaml)
"""

import os
import sys
import yaml


def load_checksums(path: str) -> dict[str, str]:
    """Parse a GoReleaser checksums.txt into {filename: sha256} mapping."""
    checksums: dict[str, str] = {}
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            parts = line.split("  ", 1)
            if len(parts) != 2:
                print(f"WARNING: unexpected line in checksums file: {line!r}", file=sys.stderr)
                continue
            sha, fname = parts
            checksums[fname] = sha
    return checksums


def update_manifest(manifest_path: str, checksums: dict[str, str], version: str, base_url: str) -> None:
    """Update plugin.yaml in-place with the release version, URLs and SHA256s."""
    with open(manifest_path) as f:
        doc = yaml.safe_load(f)

    doc["spec"]["version"] = version

    missing: list[str] = []
    for platform in doc["spec"]["platforms"]:
        labels = platform["selector"]["matchLabels"]
        goos   = labels["os"]
        goarch = labels["arch"]
        ext    = "zip" if goos == "windows" else "tar.gz"
        fname  = f"kubectl-safed_{goos}_{goarch}.{ext}"

        platform["uri"]    = f"{base_url}/{fname}"
        platform["sha256"] = checksums.get(fname, "")

        if not platform["sha256"]:
            missing.append(fname)

    if missing:
        print("ERROR: missing checksums for:", file=sys.stderr)
        for f in missing:
            print(f"  {f}", file=sys.stderr)
        sys.exit(1)

    with open(manifest_path, "w") as f:
        yaml.dump(doc, f, default_flow_style=False, sort_keys=False, allow_unicode=True)


def main() -> None:
    version  = os.environ.get("VERSION", "").strip()
    repo     = os.environ.get("REPO", "pbsladek/k8s-safed")
    checksums_path = os.environ.get("CHECKSUMS", "dist/checksums.txt")
    manifest_path  = os.environ.get("MANIFEST", "plugin.yaml")

    if not version:
        print("ERROR: VERSION environment variable is required", file=sys.stderr)
        sys.exit(1)

    if not version.startswith("v"):
        print(f"WARNING: VERSION={version!r} does not start with 'v'", file=sys.stderr)

    base_url  = f"https://github.com/{repo}/releases/download/{version}"
    checksums = load_checksums(checksums_path)

    print(f"Updating {manifest_path} for {version} ({repo})")
    update_manifest(manifest_path, checksums, version, base_url)
    print(f"Done — {manifest_path} updated successfully")


if __name__ == "__main__":
    main()
