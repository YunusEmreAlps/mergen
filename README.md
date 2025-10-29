# Mergen Firecracker Toolkit

`mergenc` combines a lightweight control plane for launching Firecracker microVMs
with an optional HTTP proxy that can discover guests through the control plane.
The control plane relies on the official
[`firecracker-go-sdk`](https://github.com/firecracker-microvm/firecracker-go-sdk)
and persists machine metadata to `state.json` so that VM state survives process
restarts.

## Build the CLI

```bash
go build -o mergenc ./cmd/mergenc
```

## Run the control plane

Start the API server on port `1323` (override with `--listen` if needed):

```bash
./mergenc serve --listen :1323
```

The control plane expects the shared Firecracker artifacts to live under the
project directory:

- `images/vmlinux.bin`
- `images/rootfs.ext4`

Each VM receives its own copy of these artifacts under
`volumes/<vm-name>/`. VM-specific logs live under `logs/`, and global state is
persisted to `state.json` at the project root. Set `PROJECT_PWD` to override the
project directory (defaults to the current working directory).

### Environment variables

- `PROJECT_PWD` &mdash; root directory containing the `images`, `volumes`, and
  `logs` folders plus `state.json` (default: current working directory).
- `MERGEN_FIRECRACKER_BIN` or `FIRECRACKER_BINARY` &mdash; override the
  Firecracker binary used by the control plane (default: `firecracker` on
  `$PATH`).

## HTTP API

All endpoints are rooted at the control plane base URL (default
`http://127.0.0.1:1323`).

### `POST /create/:name`

Creates and boots a VM named `:name`. The request body can optionally tune CPU
and memory:

```bash
curl -X POST http://127.0.0.1:1323/create/demo \
  -H 'Content-Type: application/json' \
  -d '{
        "cpu_count": 2,
        "mem_size_mb": 1024
      }'
```

The response contains the VM status plus paths to the Firecracker API socket and
per-VM volume directory.

### `POST /stop/:name`

Gracefully stops a running VM. The response reports the final status and socket
path.

### `DELETE /delete/:name`

Stops (if necessary) and deletes the VM along with its per-instance files.

## CLI shortcuts

`mergenc` wraps the HTTP API with convenience commands:

```bash
./mergenc create demo
./mergenc stop demo
./mergenc delete demo
```

Use `--url` to target a remote control plane, `--cpus` to set the vCPU count,
and `--memory` to adjust memory in MiB.

## Running tests

Run the Go test suite before submitting changes:

```bash
go test ./...
```

## Optional HTTP proxy

The repository still ships the legacy proxy component. Run `./mergenc proxy
serve --help` for usage information if you need it.

## Instruction-based talimatlar (AI agent için)

1. Go kaynak dosyalarında değişiklik yaptıktan sonra `gofmt` çalıştır.
2. HTTP API uç noktalarını ve `mergenc` CLI komutlarını bu dökümantasyonda
   tarif edildiği biçimde koru; konsol özelliğini yeniden ekleme.
3. Yeni microVM'ler için `images/` klasöründeki `vmlinux.bin` ve `rootfs.ext4`
   dosyalarını `volumes/<vm-adı>/` altına kopyala.
4. Çalışan VM durumlarını `state.json` üzerinden güncel tutmayı unutma.
5. Değişiklik yaparken loglar ve volume dizinleri için `PROJECT_PWD`
   varsayımlarına sadık kal.
