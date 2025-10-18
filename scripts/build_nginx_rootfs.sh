#!/usr/bin/env bash
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    echo "This script must be run as root (try 'sudo ./scripts/build_nginx_rootfs.sh')." >&2
    exit 1
fi

BASE_ROOTFS=${BASE_ROOTFS:-images/bionic.rootfs.ext4}
OUTPUT_ROOTFS=${OUTPUT_ROOTFS:-images/bionic-nginx.ext4}
MOUNT_DIR=""
LOOP_DEVICE=""

if [[ ! -f "${BASE_ROOTFS}" ]]; then
    echo "Base rootfs not found at ${BASE_ROOTFS}" >&2
    exit 1
fi

cleanup() {
    set +e
    if [[ -n "${MOUNT_DIR}" ]] && mountpoint -q "${MOUNT_DIR}" 2>/dev/null; then
        umount "${MOUNT_DIR}" 2>/dev/null || true
    fi
    if [[ -n "${LOOP_DEVICE}" ]]; then
        losetup -d "${LOOP_DEVICE}" 2>/dev/null || true
    fi
    if [[ -n "${MOUNT_DIR}" && -d "${MOUNT_DIR}" ]]; then
        rmdir "${MOUNT_DIR}" 2>/dev/null || true
    fi
}
trap cleanup EXIT

TMP_OUTPUT="${OUTPUT_ROOTFS}.tmp"
cp "${BASE_ROOTFS}" "${TMP_OUTPUT}"

LOOP_DEVICE=$(losetup --find --show "${TMP_OUTPUT}")
MOUNT_DIR=$(mktemp -d /tmp/mergen-rootfs.XXXXXX)
mount "${LOOP_DEVICE}" "${MOUNT_DIR}"

mkdir -p "${MOUNT_DIR}/etc"
cp /etc/resolv.conf "${MOUNT_DIR}/etc/resolv.conf"

chroot "${MOUNT_DIR}" /bin/bash <<'CHROOT_EOF'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y nginx
update-rc.d nginx defaults || true
CHROOT_EOF

sync
umount "${MOUNT_DIR}"
losetup -d "${LOOP_DEVICE}"
LOOP_DEVICE=""
rmdir "${MOUNT_DIR}"
MOUNT_DIR=""

mv "${TMP_OUTPUT}" "${OUTPUT_ROOTFS}"

cat <<SUMMARY
Created ${OUTPUT_ROOTFS} with nginx pre-installed.
Use this path as your root_drive when launching a microVM.
SUMMARY
