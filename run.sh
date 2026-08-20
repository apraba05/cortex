#!/usr/bin/env bash
# One-command demo: Redis graph ← YAML → Go scorecard API → MCP → LangChain agent.
set -euo pipefail
cd "$(dirname "$0")"
ROOT="$(pwd)"

REDIS_NAME="${REDIS_NAME:-cortex-scorecard-redis}"
REDIS_PORT="${REDIS_PORT:-6381}"
API_PORT="${API_PORT:-8091}"
export REDIS_ADDR="${REDIS_ADDR:-127.0.0.1:${REDIS_PORT}}"
export LISTEN="${LISTEN:-:${API_PORT}}"
export SCORECARD_API="${SCORECARD_API:-http://127.0.0.1:${API_PORT}}"
export MANIFEST_DIR="${MANIFEST_DIR:-services}"
API_PID=""

need_docker() {
  if docker info >/dev/null 2>&1; then
    return 0
  fi
  if sg docker -c 'docker info' >/dev/null 2>&1; then
    exec sg docker -c "\"$0\" $*"
  fi
  echo "Docker is required (and your user must reach the docker socket)." >&2
  exit 1
}

load_secrets() {
  # Later files win. Repo .env (real keys) must beat ~/.config templates
  # that often ship ANTHROPIC_API_KEY= empty.
  for f in \
    "${HOME}/.config/startup-demos.env" \
    "${ROOT}/../../.env" \
    "${ROOT}/../../../.env" \
    "${ROOT}/.env"; do
    if [[ -f "$f" ]]; then
      set -a
      # shellcheck disable=SC1090
      source "$f"
      set +a
    fi
  done
  export AWS_REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"
  export AWS_DEFAULT_REGION="$AWS_REGION"
}

cleanup() {
  [[ -n "${API_PID}" ]] && kill "${API_PID}" 2>/dev/null || true
  docker rm -f "${REDIS_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

need_docker "$@"
need_bin() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
need_bin go
need_bin python3.11
load_secrets

echo "==> Redis (${REDIS_NAME} on :${REDIS_PORT})"
docker rm -f "${REDIS_NAME}" >/dev/null 2>&1 || true
docker run -d --name "${REDIS_NAME}" -p "${REDIS_PORT}:6379" redis:7-alpine >/dev/null
for _ in $(seq 1 40); do
  if docker exec "${REDIS_NAME}" redis-cli PING 2>/dev/null | grep -q PONG; then
    break
  fi
  sleep 0.25
done
docker exec "${REDIS_NAME}" redis-cli PING | grep -q PONG

echo "==> Go modules + build scorecard API"
go mod tidy
CGO_ENABLED=0 go build -o scorecard-api .

echo "==> Python venv (MCP + LangChain)"
if [[ ! -d .venv ]]; then
  python3.11 -m venv .venv
fi
# shellcheck disable=SC1091
source .venv/bin/activate
pip install -q -r requirements.txt

echo "==> Start scorecard API ${SCORECARD_API}"
REDIS_ADDR="${REDIS_ADDR}" LISTEN="${LISTEN}" MANIFEST_DIR="${MANIFEST_DIR}" \
  ./scorecard-api > /tmp/cortex-scorecard-api.log 2>&1 &
API_PID=$!

for _ in $(seq 1 40); do
  if curl -sf "${SCORECARD_API}/health" >/dev/null; then
    break
  fi
  sleep 0.25
done
if ! curl -sf "${SCORECARD_API}/health" >/dev/null; then
  echo "scorecard API failed to start:" >&2
  cat /tmp/cortex-scorecard-api.log >&2 || true
  exit 1
fi

echo
echo "========== RAW GRAPH (services + edges) =========="
curl -s "${SCORECARD_API}/services" | python3.11 -m json.tool
echo

echo "========== SCORECARDS =========="
curl -s "${SCORECARD_API}/scorecard" | python3.11 -m json.tool
echo

echo "========== WHY NOT READY =========="
curl -s "${SCORECARD_API}/why_not_ready" | python3.11 -m json.tool
echo

echo "==> Agent via MCP tools"
SCORECARD_API="${SCORECARD_API}" python agent.py

echo
echo "Done. Redis edges sample:"
docker exec "${REDIS_NAME}" redis-cli SMEMBERS services
docker exec "${REDIS_NAME}" redis-cli SMEMBERS edge:infra:checkout-web
