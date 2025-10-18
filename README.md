# Mergen Firecracker Toolkit

This repository provides two small Go services for driving Firecracker microVMs:

- a control plane API that launches VMs, tracks their lifecycle, and records the
  guests' HTTP endpoints; and
- a host-level reverse proxy that maps incoming domains (for example,
  `app1.localhost`) to the appropriate Firecracker guest.

Both services ship in a single CLI called `mergencli` so that you can run the
control plane and the proxy as separate processes on the same machine.【F:cmd/mergencli/main.go†L1-L120】

## Prerequisites

1. A KVM-capable Linux host with the Firecracker binary installed. If the
   executable is not on your `$PATH`, set `MERGEN_FIRECRACKER_BIN` (or
   `FIRECRACKER_BINARY`) to the absolute path before starting the control
   plane.【F:internal/controlplane/manager.go†L77-L110】
2. Firecracker kernel (`vmlinux.bin`) and root filesystem (`rootfs.ext4`)
   artifacts. Place the pristine downloads under `./images` and create
   per-VM writable copies under `./volumes` as shown below. If your kernel
   artifact is gzip-compressed (for example, `vmlinux.bin.gz` or `vmlinuz`),
   the control plane will automatically decompress it into a temporary
   location before starting the VM.【F:internal/controlplane/manager.go†L109-L311】
   You can either prepare the rootfs manually (see below) or let the control
   plane synthesize one from a Docker/OCI image on demand.
3. For container-image imports, install the Docker CLI (or a compatible
   implementation that supports `docker pull`, `docker create`, and
   `docker export`) and ensure `mke2fs`/`tar` are available. The control plane
   shells out to these tools when turning container layers into writable ext4
   volumes.【F:internal/controlplane/manager.go†L312-L520】
4. Go 1.20 or newer.

## Repository layout for images and volumes

Keep the base artifacts in `images/` and create writable clones in `volumes/`.
This keeps the repository organized and mirrors the parameters that the control
plane expects:

```
mergen/
├── images/   # read-only kernel + base rootfs
└── volumes/  # per-VM writable drives copied from images/
```

Prepare the folders and download the Firecracker demo artifacts once:

```bash
mkdir -p images volumes
curl -L -o images/vmlinux.bin \
  https://s3.amazonaws.com/spec.ccfc.min/img/hello/kernel/vmlinux.bin
curl -L -o images/rootfs.ext4 \
  https://s3.amazonaws.com/spec.ccfc.min/img/hello/fsfiles/hello-rootfs.ext4
```

If you plan to run standard Ubuntu services (for example, Nginx), fetch the
larger Bionic filesystem that ships with `apt` and a full userspace:

```bash
curl -L -o images/bionic.rootfs.ext4 \
  https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/rootfs/bionic.rootfs.ext4
```

Before creating a VM, copy the base rootfs so that each guest receives its own
writable disk:

```bash
VM_ID=test1
cp images/rootfs.ext4 "volumes/${VM_ID}.ext4"
```

Use the copy (`./volumes/test1.ext4` in this example) as the
`root_drive_path` when calling the control plane or the helper scripts.

## Launching microVMs from container images

If you would rather boot directly from a Docker/OCI image, omit
`root_drive_path` and provide `container_image_url` instead. The control plane
will:

1. `docker pull` and `docker export` the requested image into a temporary
   directory.
2. Merge the image's `Entrypoint`, `Cmd`, environment, and working directory
   with any overrides you pass via `container_command`, `container_env`, and
   `container_workdir`.
3. Generate an `ext4` root disk inside `${MERGEN_STATE_DIR}/rootfs/<id>.ext4`
   using `mke2fs -d`, replacing `/sbin/init` with a lightweight launcher that
   executes the resolved command on boot.【F:internal/controlplane/manager.go†L312-L520】

The synthesized disk is deleted automatically when you remove the VM. Because
the bootstrapper reuses container metadata, you can run any image that ships a
POSIX-compatible userspace (for example, `nginx:latest` or
`ghcr.io/user/webapp:1.0`). To override the command, pass a JSON array in
`container_command`:

```bash
export CONTAINER_IMAGE_URL=nginx:latest
export CONTAINER_CMD_JSON='["nginx","-g","daemon off;"]'
./scripts/test1.sh
```

To inject additional environment variables, provide a JSON array in
`container_env` (values later in the list override the image defaults). You can
also set `container_workdir` if the image expects a specific working directory.
The resolved values are echoed back in the create response and through
`GET /machines/:id` so the proxy and observability tooling can inspect the
launch configuration.【F:internal/controlplane/manager.go†L170-L340】【F:internal/controlplane/manager.go†L360-L420】

## Baking Nginx into the root filesystem

Because the control plane cannot execute arbitrary bootstrap commands inside a
microVM, you should prepare a root filesystem that already contains your
application. The repository includes `scripts/build_nginx_rootfs.sh`, which
clones a base Ubuntu Bionic image, installs Nginx, and configures it to start
on boot so that HTTP traffic is ready as soon as the VM receives an IP address.

Run the helper as `root` (or via `sudo`):

```bash
sudo BASE_ROOTFS=images/bionic.rootfs.ext4 OUTPUT_ROOTFS=images/bionic-nginx.ext4 \
  ./scripts/build_nginx_rootfs.sh
```

The script performs the following steps:

1. Copies `images/bionic.rootfs.ext4` to `images/bionic-nginx.ext4`.
2. Mounts the copy, injects your host resolver so `apt` works, and installs the
   `nginx` package with its SysV init hooks enabled.
3. Unmounts and detaches the loop device so the result is ready for cloning.

To launch guests from this template, create a writable copy per VM as before:

```bash
VM_ID=app1
cp images/bionic-nginx.ext4 "volumes/${VM_ID}.ext4"
export ROOT_DRIVE_PATH=$(pwd)/volumes/${VM_ID}.ext4
```

After the guest acquires networking, hitting the recorded `guest_http_url`
should return the default Nginx welcome page. For example, if the microVM is
reachable on `http://172.16.0.10`, the proxy route for `app1.localhost` will
forward browser requests directly to that welcome screen.

## Building the CLI

From the repository root:

```bash
go build -o mergencli ./cmd/mergencli
```

This produces a single binary with two subcommands: `control-plane` and
`proxy`. Run each one in its own terminal so that the control plane and the
reverse proxy stay independent.【F:cmd/mergencli/main.go†L15-L120】

## Running the control plane

Start the API server on port `1323` (override with `--listen` if desired):

```bash
./mergencli control-plane serve --listen :1323
```

The control plane uses the following environment variables:

- `MERGEN_STATE_DIR`: where to store Firecracker API sockets and log files
  (defaults to `${TMPDIR}/mergen`).【F:internal/controlplane/manager.go†L77-L110】
- `MERGEN_FIRECRACKER_BIN` / `FIRECRACKER_BINARY`: override the Firecracker
  binary path if it is not simply `firecracker` in your `$PATH`.【F:internal/controlplane/manager.go†L92-L110】

### API overview

The Echo-based server exposes a health probe and three lifecycle endpoints:

- `GET /health` returns `{ "status": "ok" }`.
- `POST /machines` launches a VM using the supplied payload.
- `GET /machines/:id` refreshes status information from the Firecracker socket
  when possible.
- `DELETE /machines/:id` stops the VM, terminates the Firecracker process if
  needed, and removes the socket/log files.【F:internal/controlplane/server.go†L15-L87】【F:internal/controlplane/manager.go†L210-L360】

Each create attempt emits a structured `events` timeline that captures backend
stages such as launching Firecracker, waiting for the API socket (up to 60
seconds), pushing configuration, and surfacing any errors. Successful responses
and error payloads both include this timeline so you can troubleshoot long
boots or misconfigurations directly from the API.【F:internal/controlplane/server.go†L33-L63】【F:internal/controlplane/manager.go†L118-L330】

Every `POST /machines` payload supports the original kernel/rootfs/CPU/memory
fields plus optional guest networking metadata that the proxy consumes later:

```json
{
  "id": "machine-app1",
  "kernel_image_path": "./images/vmlinux.bin",
  "root_drive_path": "./volumes/machine-app1.ext4",
  "cpu_count": 1,
  "mem_size_mb": 512,
  "guest_address": "172.16.0.10",
  "guest_http_port": 8080,
  "guest_http_url": "http://172.16.0.10:8080"
}
```

If you omit `guest_http_url`, the control plane will build it from
`guest_address` and `guest_http_port` (defaulting to port 80). The address and
URL are surfaced on `GET /machines/:id` so that the proxy can discover them
later.【F:internal/controlplane/manager.go†L32-L206】【F:internal/controlplane/manager.go†L244-L293】

To boot directly from a registry image, skip `root_drive_path` and pass
`"container_image_url": "nginx:latest"` along with any of the optional
`container_*` overrides described above. The create response and subsequent
status calls will echo the resolved command, environment, working directory,
and the synthesized root drive path so that operators can audit the launch
configuration.【F:internal/controlplane/manager.go†L170-L420】

### Helper scripts

The `scripts/` folder includes two `curl`-based helpers for quick manual tests:

- `scripts/test1.sh` validates your inputs, builds a JSON payload, and sends
  `POST /machines`. Provide either `ROOT_DRIVE_PATH` or
  `CONTAINER_IMAGE_URL`, and optionally override the container metadata via
  `CONTAINER_CMD_JSON`, `CONTAINER_ENV_JSON`, or `CONTAINER_WORKDIR`.
  Networking hints remain the same: set `GUEST_ADDRESS`, `GUEST_HTTP_PORT`, or
  `GUEST_HTTP_URL` to record how the guest will be reachable.【F:scripts/test1.sh†L1-L70】
- `scripts/test2.sh` fetches the latest status and optionally issues
  `DELETE /machines/:id` (disable deletion with `DELETE_AFTER_STATUS=false`).【F:scripts/test2.sh†L1-L21】

Example session:

```bash
export API_URL=http://127.0.0.1:1323
export VM_ID=machine-app1
export KERNEL_IMAGE_PATH=$(pwd)/images/vmlinux.bin
export ROOT_DRIVE_PATH=$(pwd)/volumes/${VM_ID}.ext4
export GUEST_ADDRESS=172.16.0.10
export GUEST_HTTP_PORT=8080

./scripts/test1.sh   # create the VM
./scripts/test2.sh   # check status (and delete by default)
```

## Running the host proxy

Once the control plane is running, start the reverse proxy in a separate
terminal. The proxy reads a JSON configuration file that maps incoming host
names to either a machine ID (resolved through the control plane) or a static
HTTP endpoint.

Create `proxy-config.json` with routes for each domain you want to expose:

```json
{
  "listen_addr": ":8080",
  "control_plane_url": "http://127.0.0.1:1323",
  "routes": {
    "app1.localhost": { "machine_id": "machine-app1" },
    "app2.localhost": { "machine_id": "machine-app2" }
  }
}
```

Launch the proxy:

```bash
./mergencli proxy serve --config proxy-config.json
```

Incoming requests are matched against the `Host` header. For each match, the
proxy queries the control plane for the machine's recorded guest endpoint and
forwards HTTP traffic accordingly. You can override the listen address with
`--listen` or the control plane location with `--control-plane-url`. A built-in
health probe responds on `/healthz`.【F:internal/proxy/proxy.go†L21-L174】 Set
`LOG_LEVEL=debug` (or `LOG_LEVEL=1`) before launching the proxy if you want to
emit access logs that include the resolved backend, status code, and latency for
every request.【F:internal/proxy/proxy.go†L57-L132】

With this setup, visiting `http://app1.localhost:8080` will forward traffic to
`machine-app1` as soon as the VM advertises a `guest_address` or
`guest_http_url` via the control plane.【F:internal/proxy/proxy.go†L176-L209】

## Development tips

- The control plane keeps state in memory. Restarting the process clears the
  registry of VMs, so plan to recreate or reconcile machines on boot.
- Both services honor `SIGINT`/`SIGTERM` for graceful shutdowns, making it easy
  to run them under `systemd` or a supervisor of your choice.【F:cmd/mergencli/main.go†L88-L120】【F:internal/proxy/proxy.go†L95-L118】
