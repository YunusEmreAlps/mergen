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
FIRECRACKER_BINARY=${FIRECRACKER_BINARY:-}

if [[ ! -f "${KERNEL_IMAGE_PATH}" ]]; then
    echo "Kernel image not found at ${KERNEL_IMAGE_PATH}" >&2
    exit 1
fi

if [[ ! -f "${ROOT_DRIVE_PATH}" ]]; then
    echo "Root drive not found at ${ROOT_DRIVE_PATH}" >&2
    exit 1
fi

JSON_PAYLOAD=$(VM_ID="$VM_ID" \
    KERNEL_IMAGE_PATH="$KERNEL_IMAGE_PATH" \
    ROOT_DRIVE_PATH="$ROOT_DRIVE_PATH" \
    BOOT_ARGS="$BOOT_ARGS" \
    CPU_COUNT="$CPU_COUNT" \
    MEM_SIZE_MB="$MEM_SIZE_MB" \
    CONTAINER_IMAGE_URL="$CONTAINER_IMAGE_URL" \
    FIRECRACKER_BINARY="$FIRECRACKER_BINARY" \
    python3 - <<'PY'
import json
import os

payload = {
    "id": os.environ["VM_ID"],
    "kernel_image_path": os.environ["KERNEL_IMAGE_PATH"],
    "root_drive_path": os.environ["ROOT_DRIVE_PATH"],
    "boot_args": os.environ["BOOT_ARGS"],
    "cpu_count": int(os.environ["CPU_COUNT"]),
    "mem_size_mb": int(os.environ["MEM_SIZE_MB"]),
}
if os.environ.get("CONTAINER_IMAGE_URL"):
    payload["container_image_url"] = os.environ["CONTAINER_IMAGE_URL"]
if os.environ.get("FIRECRACKER_BINARY"):
    payload["firecracker_binary"] = os.environ["FIRECRACKER_BINARY"]

print(json.dumps(payload))
PY
)

echo "Creating microVM ${VM_ID}..."
curl -sS -X POST "${API_URL}/machines" \
    -H 'Content-Type: application/json' \
    -d "${JSON_PAYLOAD}" | python3 -m json.tool
