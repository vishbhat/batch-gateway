#!/usr/bin/env bash
# Run batch-gateway E2E tests against an RHOAI deployment.
# Enables all tests that are practical without inference simulators (sim-model*).
#
# Prerequisites:
#   - oc logged in to the target cluster
#   - batch-gateway deployed (see docs/guides/deploy-rhoai.md)
#   - helm upgrade applied with e2e-friendly settings (pass-through headers,
#     pprof, fast GC interval) — run this script's setup step or:
#       helm upgrade batch-gateway ./charts/batch-gateway -n batch-api --reuse-values \
#         --set 'apiserver.config.batchAPI.passThroughHeaders={Authorization,X-E2E-Pass-Through-1,X-E2E-Pass-Through-2}' \
#         --set apiserver.config.enablePprof=true \
#         --set processor.config.enablePprof=true \
#         --set gc.config.interval=30s \
#         --set processor.logging.verbosity=3
#
# Usage:
#   ./scripts/test-e2e-rhoai.sh              # full suite
#   ./scripts/test-e2e-rhoai.sh TestE2E/Batches/Lifecycle   # filter via TEST_RUN

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

BATCH_NS="${BATCH_NS:-batch-api}"
HELM_RELEASE="${HELM_RELEASE:-batch-gateway}"
LLM_NS="${LLM_NS:-llm}"
GW_HOST="${GW_HOST:-llm-inference.apps.batch-dis-rhoai-test.aws.rh-ods.com}"
TEST_MODEL="${TEST_MODEL:-facebook/opt-125m}"
TEST_SA="${TEST_SA:-test-authorized-sa}"

APISERVER_PF_LOCAL="${APISERVER_PF_LOCAL:-8081}"
PROCESSOR_PF_LOCAL="${PROCESSOR_PF_LOCAL:-9091}"

if [[ "${1:-}" == "--setup-only" ]]; then
  exec helm upgrade "${HELM_RELEASE}" "${REPO_ROOT}/charts/batch-gateway" -n "${BATCH_NS}" --reuse-values \
    --set 'apiserver.config.batchAPI.passThroughHeaders={Authorization,X-E2E-Pass-Through-1,X-E2E-Pass-Through-2}' \
    --set apiserver.config.enablePprof=true \
    --set processor.config.enablePprof=true \
    --set gc.config.interval=30s \
    --set processor.logging.verbosity=3 \
    --wait --timeout=5m
fi

PF_PIDS=()
cleanup() {
  for pid in "${PF_PIDS[@]}"; do
    kill "${pid}" 2>/dev/null || true
  done
}
trap cleanup EXIT

echo "Starting observability port-forwards..."
oc port-forward -n "${BATCH_NS}" "deploy/${HELM_RELEASE}-apiserver" "${APISERVER_PF_LOCAL}:8081" &
PF_PIDS+=($!)
oc port-forward -n "${BATCH_NS}" "deploy/${HELM_RELEASE}-processor" "${PROCESSOR_PF_LOCAL}:9090" &
PF_PIDS+=($!)

# Wait for /ready on both observability endpoints.
for i in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:${APISERVER_PF_LOCAL}/ready" >/dev/null \
    && curl -sf "http://127.0.0.1:${PROCESSOR_PF_LOCAL}/ready" >/dev/null; then
    break
  fi
  sleep 1
done

export TEST_CLUSTER_SERVER
TEST_CLUSTER_SERVER="$(oc whoami --show-server)"
export TEST_APISERVER_URL="https://${GW_HOST}"
export TEST_APISERVER_OBS_URL="http://127.0.0.1:${APISERVER_PF_LOCAL}"
export TEST_PROCESSOR_OBS_URL="http://127.0.0.1:${PROCESSOR_PF_LOCAL}"
export TEST_BEARER_TOKEN
TEST_BEARER_TOKEN="$(oc create token "${TEST_SA}" -n "${LLM_NS}" \
  --audience=https://kubernetes.default.svc --duration=30m)"
export TEST_NAMESPACE="${BATCH_NS}"
export TEST_HELM_RELEASE="${HELM_RELEASE}"
export TEST_MODEL
export TEST_INFERENCE_OBJECTIVE="${TEST_INFERENCE_OBJECTIVE:-batch-sheddable}"
export TEST_CHART_PATH="${REPO_ROOT}/charts/batch-gateway"

# Keep skips for tests that need simulators or a second model.
export TEST_SKIP_MULTIMODEL=true
export TEST_SKIP_TIMING_TESTS=true

# Helm upgrade e2e is safe when processor is helm-managed (not kubectl-patched).
unset TEST_SKIP_HELM_UPGRADE

if [[ -n "${1:-}" ]]; then
  export TEST_RUN="$1"
fi

cd "${REPO_ROOT}"
make test-e2e
