#!/usr/bin/env bash
# commit-manifest.sh
#
# Commits an updated plugin.yaml to the current branch.
# Idempotent: exits cleanly if there are no staged changes.
#
# Usage:
#   VERSION=v0.1.0 bash scripts/commit-manifest.sh
#
# Environment variables:
#   VERSION  - the release tag used in the commit message (required)

set -euo pipefail

: "${VERSION:?VERSION environment variable is required}"

git config user.name  "github-actions[bot]"
git config user.email "github-actions[bot]@users.noreply.github.com"

git add plugin.yaml

if git diff --cached --quiet; then
  echo "plugin.yaml unchanged — nothing to commit"
  exit 0
fi

git commit -m "chore: update krew manifest for ${VERSION} [skip ci]"
git push origin HEAD:main

echo "plugin.yaml committed and pushed for ${VERSION}"
