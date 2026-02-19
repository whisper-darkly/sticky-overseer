#!/usr/bin/env bash
# Docker smoke test for sticky-overseer.
# Builds docker/Dockerfile and runs a simple WebSocket round-trip
# using /bin/sh as the pinned command.
#
# Usage: bash docker/test.sh
# Requires: docker, and either websocat or python3 with websockets package.
set -euo pipefail

IMAGE="sticky-overseer-test"
CONTAINER="so-test"
PORT=18080
WS_URL="ws://localhost:${PORT}/"

# Cleanup on exit
cleanup() {
    docker rm -f "${CONTAINER}" 2>/dev/null || true
    docker rmi -f "${IMAGE}" 2>/dev/null || true
}
trap cleanup EXIT

echo "==> Building image ${IMAGE} (Dockerfile.base)..."
docker build -f docker/Dockerfile -t "${IMAGE}" .

echo "==> Starting container ${CONTAINER} (pinned to /bin/sh)..."
docker run -d --rm \
    -p "${PORT}:8080" \
    -e OVERSEER_TRUSTED_CIDRS="" \
    --name "${CONTAINER}" \
    "${IMAGE}" \
    sticky-overseer -command /bin/sh

echo "==> Waiting for healthcheck to pass..."
DEADLINE=$(( $(date +%s) + 60 ))
while true; do
    STATUS=$(docker inspect --format='{{.State.Health.Status}}' "${CONTAINER}" 2>/dev/null || echo "missing")
    if [ "${STATUS}" = "healthy" ]; then
        echo "    Container is healthy."
        break
    fi
    if [ "$(date +%s)" -ge "${DEADLINE}" ]; then
        echo "ERROR: container did not become healthy within 60s (status: ${STATUS})"
        docker logs "${CONTAINER}" || true
        exit 1
    fi
    sleep 2
done

echo "==> Sending WebSocket start message..."

# Command: sh -c 'while true; do echo hello; sleep 10; done'
START_MSG='{"type":"start","id":"smoke1","args":["-c","while true; do echo hello; sleep 10; done"]}'
RESPONSE=""

if command -v websocat &>/dev/null; then
    echo "    Using websocat..."
    RESPONSE=$(echo "${START_MSG}" | timeout 10 websocat --no-close -n1 "${WS_URL}" 2>/dev/null || true)
elif command -v python3 &>/dev/null && python3 -c "import websockets" 2>/dev/null; then
    echo "    Using python3 websockets..."
    RESPONSE=$(python3 - <<PYEOF
import asyncio, websockets

async def run():
    async with websockets.connect("${WS_URL}") as ws:
        await ws.send('${START_MSG}')
        msg = await asyncio.wait_for(ws.recv(), timeout=5)
        print(msg)

asyncio.run(run())
PYEOF
)
else
    echo "SKIP: neither websocat nor python3+websockets available; skipping WS round-trip"
    echo "      Run 'make test-tools' from the repo root to install them."
    echo "==> Smoke test PASSED (partial — WS round-trip skipped)"
    exit 0
fi

echo "    Response: ${RESPONSE}"

if echo "${RESPONSE}" | grep -q '"started"'; then
    echo "==> Smoke test PASSED"
else
    echo "ERROR: expected 'started' in response, got: ${RESPONSE}"
    exit 1
fi
