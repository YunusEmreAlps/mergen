# Mergen Firecracker Control Plane

This repository exposes a small HTTP control plane that can create, inspect, and
tear down Firecracker microVMs. The service launches the Firecracker binary,
pushes the machine configuration over its REST API, and tracks lifecycle state
in memory so that callers can query `/machines/:id` for status or issue deletes
when a VM is no longer needed.【F:main.go†L30-L258】【F:main.go†L507-L560】

The server listens on port `1323` by default and provides:

- `POST /machines` — Launch a microVM based on the supplied kernel, rootfs,
  CPU/memory, and optional metadata.
- `GET /machines/:id` — Read live status from the Firecracker API socket when
  possible and fall back to the tracked process state.
- `DELETE /machines/:id` — Ask Firecracker to stop, fall back to a SIGTERM, and
  remove the associated Unix socket and log file.【F:main.go†L507-L560】

## Prerequisites

1. A host with KVM support and the Firecracker binary installed (e.g. following
   the instructions in `instructions.sh`). If the binary is not on your `$PATH`,
   set `MERGEN_FIRECRACKER_BIN` (or `FIRECRACKER_BINARY`) to the absolute path
   before starting the service.【F:main.go†L75-L103】【F:main.go†L160-L167】
2. A kernel image (`vmlinux.bin`) and a root filesystem (`rootfs.ext4`) that are
   compatible with Firecracker.【F:main.go†L110-L217】
3. Go 1.20+ to build or run the control plane.

## Repository layout for images and volumes

To keep downloads and writable disks organized inside this repo, use the
following structure:

```
mergen/
├── images/   # read-only assets shared by many VMs (kernel + pristine rootfs)
└── volumes/  # per-VM writable copies of the base rootfs
```

Create these directories once:

```bash
mkdir -p images volumes
```

### Downloading base artifacts

You can reuse the artifacts from the official Firecracker quickstart:

```bash
curl -L -o images/vmlinux.bin \
  https://s3.amazonaws.com/spec.ccfc.min/img/hello/kernel/vmlinux.bin
curl -L -o images/rootfs.ext4 \
  https://s3.amazonaws.com/spec.ccfc.min/img/hello/fsfiles/hello-rootfs.ext4
```

Keep the files in `images/` read-only so you can clone them for each VM you
launch.

### Preparing per-VM writable disks

Before creating a VM, copy the base rootfs into `volumes/` and point the API
request at the copy. Each Firecracker microVM needs its own writable block
device so that state changes do not modify the shared base image:

```bash
VM_ID=test1
cp images/rootfs.ext4 "volumes/${VM_ID}.ext4"
```

Use the new path (`./volumes/test1.ext4` in this example) as the
`root_drive_path` when calling the API or running the helper scripts.【F:main.go†L212-L258】

## Running the control plane

From the repository root:

```bash
go run .
```

Optional environment variables:

- `MERGEN_STATE_DIR`: directory that will receive Firecracker sockets and log
  files. Defaults to `${TMPDIR}/mergen` if unset.【F:main.go†L75-L155】
- `MERGEN_FIRECRACKER_BIN` (or `FIRECRACKER_BINARY`): override the Firecracker
  binary path if it is not just `firecracker` in your `$PATH`.【F:main.go†L89-L167】

The service exposes a simple health probe at `GET /health` and the machine
lifecycle endpoints listed above.【F:main.go†L514-L560】

## Testing the endpoints with scripts

Two helper scripts under `scripts/` exercise the API using `curl`:

- `scripts/test1.sh` — posts JSON to `/machines` with the kernel/rootfs paths
  you provide via environment variables. It validates that the files exist
  before sending the request.【F:scripts/test1.sh†L4-L56】
- `scripts/test2.sh` — fetches `/machines/:id` and optionally deletes the VM.
  Set `DELETE_AFTER_STATUS=false` if you want to keep it running.【F:scripts/test2.sh†L4-L22】

Example usage, assuming you followed the directory layout above:

```bash
export API_URL=http://127.0.0.1:1323
export VM_ID=test1
export KERNEL_IMAGE_PATH=$(pwd)/images/vmlinux.bin
export ROOT_DRIVE_PATH=$(pwd)/volumes/${VM_ID}.ext4

./scripts/test1.sh   # create the VM
./scripts/test2.sh   # read status and delete it (default behavior)
```

If you would like to map a VM back to a container image or other metadata,
include `CONTAINER_IMAGE_URL` in the environment when running `test1.sh`. The
value is stored alongside the machine status and surfaced in the API response.【F:main.go†L30-L258】
