#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v mise >/dev/null 2>&1; then
  echo "mise is required for the repo-pinned Go toolchain" >&2
  exit 1
fi

go_root="$(cd "${repo_root}" && mise where go)"
go_bin="${go_root}/bin/go"

if [ ! -x "${go_bin}" ]; then
  echo "mise Go is not installed at ${go_bin}; run: mise install" >&2
  exit 1
fi

export GOROOT="${go_root}"
export PATH="${go_root}/bin:${PATH}"
export SAFED_E2E_GO="${go_bin}"

exec "$@"
