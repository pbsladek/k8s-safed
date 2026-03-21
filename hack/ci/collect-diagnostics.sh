#!/usr/bin/env bash
# collect-diagnostics.sh — capture cluster state for post-failure analysis.
# Called automatically by the e2e GitHub workflow on job failure.
#
# Output directory: /tmp/safed-e2e-diagnostics

set -euo pipefail

CLUSTER="${SAFED_E2E_CLUSTER_NAME:-safed-e2e-ci}"
OUT="/tmp/safed-e2e-diagnostics"

mkdir -p "${OUT}"

echo "[diagnostics] Collecting cluster state for cluster: ${CLUSTER}"

# Locate the kubeconfig written by k3d.
KUBECONFIG_FILE=$(mktemp /tmp/safed-diag-kubeconfig-XXXXXX.yaml)
trap 'rm -f "${KUBECONFIG_FILE}"' EXIT

if ! k3d kubeconfig write "${CLUSTER}" --output "${KUBECONFIG_FILE}" 2>/dev/null; then
  echo "[diagnostics] Could not get kubeconfig — cluster may already be deleted." >&2
  exit 0
fi

KC="kubectl --kubeconfig=${KUBECONFIG_FILE}"

# ── Nodes ──────────────────────────────────────────────────────────────────────
echo "[diagnostics] nodes..."
${KC} get nodes -o wide > "${OUT}/nodes.txt" 2>&1 || true
${KC} describe nodes   > "${OUT}/nodes-describe.txt" 2>&1 || true

# ── All resources ──────────────────────────────────────────────────────────────
echo "[diagnostics] all resources..."
${KC} get all --all-namespaces -o wide > "${OUT}/all-resources.txt" 2>&1 || true

# ── e2e namespace detail ───────────────────────────────────────────────────────
echo "[diagnostics] e2e namespace..."
${KC} get all -n e2e -o yaml > "${OUT}/e2e-all.yaml"      2>&1 || true
${KC} describe pods -n e2e   > "${OUT}/e2e-pods-describe.txt" 2>&1 || true

# ── Events ────────────────────────────────────────────────────────────────────
echo "[diagnostics] events..."
${KC} get events --all-namespaces --sort-by='.lastTimestamp' > "${OUT}/events.txt" 2>&1 || true

# ── Pod logs ──────────────────────────────────────────────────────────────────
echo "[diagnostics] pod logs..."
mkdir -p "${OUT}/logs"
for pod in $(${KC} get pods -n e2e -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
  ${KC} logs -n e2e "${pod}" --all-containers --prefix \
    > "${OUT}/logs/${pod}.txt" 2>&1 || true
  ${KC} logs -n e2e "${pod}" --all-containers --prefix --previous \
    > "${OUT}/logs/${pod}-previous.txt" 2>&1 || true
done

echo "[diagnostics] Done. Files written to ${OUT}/"
find "${OUT}" -type f | sort
