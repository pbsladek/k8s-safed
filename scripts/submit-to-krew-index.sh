#!/usr/bin/env bash
# submit-to-krew-index.sh
#
# Opens a pull request against kubernetes-sigs/krew-index to publish or update
# the plugin manifest. Requires a fork of krew-index and a GitHub PAT.
#
# One-time setup (do this manually before the first release):
#   1. Fork https://github.com/kubernetes-sigs/krew-index on GitHub.
#   2. Create a GitHub PAT with the "repo" scope at
#      https://github.com/settings/tokens/new
#   3. Add the PAT as a repository secret named KREW_INDEX_PR_TOKEN.
#   4. Set the KREW_INDEX_FORK variable below (or as a repository variable).
#
# Usage:
#   KREW_GITHUB_TOKEN=<pat> VERSION=v0.1.0 bash scripts/submit-to-krew-index.sh
#
# Environment variables:
#   KREW_GITHUB_TOKEN  - PAT with repo scope on your krew-index fork (required)
#   VERSION            - release tag, e.g. v0.1.0                    (required)
#   PLUGIN_NAME        - krew plugin name                            (default: safed)
#   KREW_INDEX_FORK    - your fork slug, e.g. pbsladek/krew-index   (required)
#   MANIFEST           - path to the updated plugin manifest          (default: plugin.yaml)

set -euo pipefail

: "${KREW_GITHUB_TOKEN:?KREW_GITHUB_TOKEN is required — set a PAT with repo scope}"
: "${VERSION:?VERSION is required, e.g. v0.1.0}"
: "${KREW_INDEX_FORK:?KREW_INDEX_FORK is required, e.g. pbsladek/krew-index}"

PLUGIN_NAME="${PLUGIN_NAME:-safed}"
MANIFEST="${MANIFEST:-plugin.yaml}"
UPSTREAM="kubernetes-sigs/krew-index"
BRANCH="update-${PLUGIN_NAME}-${VERSION}"
WORKDIR="$(mktemp -d)"

echo "Submitting ${PLUGIN_NAME} ${VERSION} to ${UPSTREAM}"
echo "  Fork:    ${KREW_INDEX_FORK}"
echo "  Branch:  ${BRANCH}"
echo "  Workdir: ${WORKDIR}"

# Authenticate the GitHub CLI with the token so git push and gh pr create both work.
echo "${KREW_GITHUB_TOKEN}" | gh auth login --with-token

# Clone the fork (shallow is fine — we only need HEAD).
git clone --depth=1 "https://github.com/${KREW_INDEX_FORK}.git" "${WORKDIR}/krew-index"
cd "${WORKDIR}/krew-index"

# Sync with upstream master so the PR has no conflicts.
git remote add upstream "https://github.com/${UPSTREAM}.git"
git fetch --depth=1 upstream master
git rebase upstream/master

# Create the release branch.
git checkout -b "${BRANCH}"

# Copy the manifest into the krew-index plugins directory.
mkdir -p plugins
cp "${OLDPWD}/${MANIFEST}" "plugins/${PLUGIN_NAME}.yaml"

# Commit.
git config user.name  "github-actions[bot]"
git config user.email "github-actions[bot]@users.noreply.github.com"
git add "plugins/${PLUGIN_NAME}.yaml"
git commit -m "feat: update ${PLUGIN_NAME} to ${VERSION}"

# Push the branch to the fork.
git push origin "${BRANCH}"

# Open the PR against upstream using the GitHub CLI.
# gh reads GITHUB_TOKEN from the environment; we override it here with the PAT
# so it has permission to open a PR on the upstream repo.
GITHUB_TOKEN="${KREW_GITHUB_TOKEN}" gh pr create \
  --repo "${UPSTREAM}" \
  --head "${KREW_INDEX_FORK%%/*}:${BRANCH}" \
  --base master \
  --title "feat: update ${PLUGIN_NAME} to ${VERSION}" \
  --body "$(cat <<EOF
Automated release of \`${PLUGIN_NAME}\` **${VERSION}**.

Submitted by the [k8s-safed release workflow](https://github.com/pbsladek/k8s-safed).
EOF
)"

echo "PR opened against ${UPSTREAM}"
