#!/usr/bin/env bash
set -euo pipefail

API_URL=${API_URL:-http://127.0.0.1:1323}
VM_ID=${VM_ID:-http-bootstrap}
STARTUP_SCRIPT_ID=${STARTUP_SCRIPT_ID:-hello-http}
KERNEL_IMAGE_PATH=${KERNEL_IMAGE_PATH:-/var/lib/firecracker/vmlinux.bin}
CONTAINER_IMAGE_URL=${CONTAINER_IMAGE_URL:-docker.io/library/ubuntu:20.04}
CONTAINER_CMD_JSON=${CONTAINER_CMD_JSON:-'["/bin/sh","-c","tail -f /dev/null"]'}
CONTAINER_ENV_JSON=${CONTAINER_ENV_JSON:-'["MERGEN_HTTP_PORT=8080"]'}

if [[ ! -f "${KERNEL_IMAGE_PATH}" ]]; then
    echo "Kernel image not found at ${KERNEL_IMAGE_PATH}" >&2
    exit 1
fi

json_escape() {
    local s="$1"
    s=${s//\\/\\\\}
    s=${s//"/\\"}
    s=${s//$'\n'/\n}
    s=${s//$'\r'/\r}
    printf '%s' "$s"
}

read -r -d '' STARTUP_SCRIPT <<'SCRIPT'
#!/bin/sh
set -e
if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update >/dev/null 2>&1 || true
    apt-get install -y --no-install-recommends netcat >/dev/null 2>&1 || true
fi
if ! command -v nc >/dev/null 2>&1; then
    echo "[mergen] warning: nc command not available; startup service cannot bind" >&2
    exit 0
fi
cat <<'HELLO' >/usr/local/bin/mergen-hello-http
#!/bin/sh
set -e
PORT="${MERGEN_HTTP_PORT:-8080}"
while true; do
    { printf 'HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 21\r\n\r\nhello from microvm!'; } | nc -l -p "${PORT}" -q 1
    sleep 1
done
HELLO
chmod +x /usr/local/bin/mergen-hello-http
MERGEN_HTTP_PORT=${MERGEN_HTTP_PORT:-8080}
/usr/local/bin/mergen-hello-http &
SCRIPT

BOOTSTRAP_PAYLOAD=$(cat <<JSON
{"id":"$(json_escape "$STARTUP_SCRIPT_ID")","script":"$(json_escape "$STARTUP_SCRIPT")"}
JSON
)

echo "Registering startup script ${STARTUP_SCRIPT_ID}..."
curl -sS -X POST "${API_URL}/bootstrap" \
    -H 'Content-Type: application/json' \
    -d "${BOOTSTRAP_PAYLOAD}"

echo "\nRequesting microVM ${VM_ID} with startup script..."
STARTUP_SCRIPT_ID="${STARTUP_SCRIPT_ID}" \
VM_ID="${VM_ID}" \
KERNEL_IMAGE_PATH="${KERNEL_IMAGE_PATH}" \
CONTAINER_IMAGE_URL="${CONTAINER_IMAGE_URL}" \
CONTAINER_CMD_JSON="$(printf '%s' "$CONTAINER_CMD_JSON")" \
CONTAINER_ENV_JSON="$(printf '%s' "$CONTAINER_ENV_JSON")" \
"$(dirname "$0")/test1.sh"

echo "\nInspecting status for ${VM_ID}..."
curl -sS "${API_URL}/machines/${VM_ID}"
