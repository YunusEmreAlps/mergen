#!/usr/bin/env bash
set -euo pipefail

API_URL=${API_URL:-http://127.0.0.1:1323}
VM_ID=${VM_ID:-demo}
KERNEL_IMAGE_PATH=${KERNEL_IMAGE_PATH:-/var/lib/firecracker/vmlinux.bin}
ROOT_DRIVE_PATH=${ROOT_DRIVE_PATH:-/var/lib/firecracker/rootfs.ext4}
BOOT_ARGS=${BOOT_ARGS:-"console=ttyS0 reboot=k panic=1 pci=off"}
CPU_COUNT=${CPU_COUNT:-1}
MEM_SIZE_MB=${MEM_SIZE_MB:-512}
CONTAINER_IMAGE_URL=${CONTAINER_IMAGE_URL:-}
CONTAINER_CMD_JSON=${CONTAINER_CMD_JSON:-}
CONTAINER_ENV_JSON=${CONTAINER_ENV_JSON:-}
CONTAINER_WORKDIR=${CONTAINER_WORKDIR:-}
FIRECRACKER_BINARY=${FIRECRACKER_BINARY:-}
GUEST_ADDRESS=${GUEST_ADDRESS:-}
GUEST_HTTP_PORT=${GUEST_HTTP_PORT:-}
GUEST_HTTP_URL=${GUEST_HTTP_URL:-}
STARTUP_SCRIPT_ID=${STARTUP_SCRIPT_ID:-}

if [[ ! -f "${KERNEL_IMAGE_PATH}" ]]; then
    echo "Kernel image not found at ${KERNEL_IMAGE_PATH}" >&2
    exit 1
fi

if [[ -z "${ROOT_DRIVE_PATH}" && -z "${CONTAINER_IMAGE_URL}" ]]; then
    echo "Either ROOT_DRIVE_PATH or CONTAINER_IMAGE_URL must be provided." >&2
    exit 1
fi

if [[ -n "${ROOT_DRIVE_PATH}" && ! -f "${ROOT_DRIVE_PATH}" ]]; then
    echo "Root drive not found at ${ROOT_DRIVE_PATH}" >&2
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

JSON_PARTS=(
    "\"id\":\"$(json_escape "$VM_ID")\""
    "\"kernel_image_path\":\"$(json_escape "$KERNEL_IMAGE_PATH")\""
    "\"boot_args\":\"$(json_escape "$BOOT_ARGS")\""
    "\"cpu_count\":${CPU_COUNT}"
    "\"mem_size_mb\":${MEM_SIZE_MB}"
)

if [[ -n "${ROOT_DRIVE_PATH}" ]]; then
    JSON_PARTS+=("\"root_drive_path\":\"$(json_escape "$ROOT_DRIVE_PATH")\"")
fi

if [[ -n "${CONTAINER_IMAGE_URL}" ]]; then
    JSON_PARTS+=("\"container_image_url\":\"$(json_escape "$CONTAINER_IMAGE_URL")\"")
fi
if [[ -n "${CONTAINER_CMD_JSON}" ]]; then
    JSON_PARTS+=("\"container_command\":${CONTAINER_CMD_JSON}")
fi
if [[ -n "${CONTAINER_ENV_JSON}" ]]; then
    JSON_PARTS+=("\"container_env\":${CONTAINER_ENV_JSON}")
fi
if [[ -n "${CONTAINER_WORKDIR}" ]]; then
    JSON_PARTS+=("\"container_workdir\":\"$(json_escape "$CONTAINER_WORKDIR")\"")
fi
if [[ -n "${FIRECRACKER_BINARY}" ]]; then
    JSON_PARTS+=("\"firecracker_binary\":\"$(json_escape "$FIRECRACKER_BINARY")\"")
fi
if [[ -n "${GUEST_ADDRESS}" ]]; then
    JSON_PARTS+=("\"guest_address\":\"$(json_escape "$GUEST_ADDRESS")\"")
fi
if [[ -n "${GUEST_HTTP_PORT}" ]]; then
    JSON_PARTS+=("\"guest_http_port\":${GUEST_HTTP_PORT}")
fi
if [[ -n "${GUEST_HTTP_URL}" ]]; then
    JSON_PARTS+=("\"guest_http_url\":\"$(json_escape "$GUEST_HTTP_URL")\"")
fi
if [[ -n "${STARTUP_SCRIPT_ID}" ]]; then
    JSON_PARTS+=("\"startup_script_id\":\"$(json_escape "$STARTUP_SCRIPT_ID")\"")
fi

JSON_JOINED=$(IFS=,; printf '%s' "${JSON_PARTS[*]}")
JSON_PAYLOAD="{${JSON_JOINED}}"

echo "Creating microVM ${VM_ID}..."
curl -sS -X POST "${API_URL}/machines" \
    -H 'Content-Type: application/json' \
    -d "${JSON_PAYLOAD}"
