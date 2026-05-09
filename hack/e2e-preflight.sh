#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

need mise
need docker

if [[ ! -f .mise.toml ]]; then
  echo "missing .mise.toml; e2e should use the repository mise toolchain" >&2
  exit 1
fi

mise exec -- go version >/dev/null
mise exec -- kubectl version --client=true >/dev/null
mise exec -- helm version --short >/dev/null
mise exec -- k3d version >/dev/null

if ! docker info >/dev/null 2>&1; then
  echo "docker is not available; k3d e2e tests require a running Docker daemon" >&2
  exit 1
fi

echo "e2e preflight ok"
