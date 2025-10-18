#!/usr/bin/env bash
set -euo pipefail

API_URL=${API_URL:-http://127.0.0.1:1323}
VM_ID=${VM_ID:-demo}
DELETE_AFTER_STATUS=${DELETE_AFTER_STATUS:-true}

if [[ -z "${VM_ID}" ]]; then
    echo "Set VM_ID to the machine identifier you created." >&2
    exit 1
fi

echo "Fetching status for ${VM_ID}..."
if ! STATUS_RESPONSE=$(curl -sS "${API_URL}/machines/${VM_ID}"); then
    echo "Unable to read status for ${VM_ID}." >&2
    exit 1
fi
printf '%s\n' "${STATUS_RESPONSE}"

if [[ "${DELETE_AFTER_STATUS}" == "true" ]]; then
    echo "Deleting ${VM_ID}..."
    curl -sS -X DELETE "${API_URL}/machines/${VM_ID}" -w '\nHTTP %\{http_code\}\n'
fi
