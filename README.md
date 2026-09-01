# cn4m-cascade — SMB Sync Engine

A self-hosted sync tool in the spirit of FreeFileSync, run from a web UI and packaged as a single
container. Sources and destinations are primarily **SMB/CIFS shares addressed by IP**, mounted on
demand by the kernel and treated as ordinary filesystem paths, so the sync engine itself never
learns about SMB.

**[SPEC.md](SPEC.md) is the source of truth** for design and scope. [CLAUDE.md](CLAUDE.md) holds the
working rules; [PROGRESS.md](PROGRESS.md) tracks what is built and what is next.

## Status

Built in phases (SPEC.md §11). **Phase 1 of 6 is complete.**

| | |
|---|---|
| ✅ Works today | Target CRUD over HTTP, on-demand kernel CIFS mounts with refcounting and an idle grace period, SMB dialect fallback, multichannel fallback, stale-mount detection, encrypted credentials, startup mount cleanup, legible failure messages |
| ⛔ Not built yet | The sync engine itself (scan/diff/copy), filters, multi-destination fan-out, the web UI, scheduling, webhooks, two-way sync |

There is no UI yet, and nothing syncs files yet. Phase 1 is the skeleton and the mount manager.

## Requirements

- **Docker** (Docker Desktop is fine). The daemon must be responsive — see [Troubleshooting](#troubleshooting).
- **Nothing else.** You do not need Go, `cifs-utils`, or a Samba install on your machine. Every
  build, lint and test runs inside the dev container, because `mount.cifs` is Linux-only and the
  host is often macOS. Every `make` target is a thin `docker compose` wrapper.

## Quickstart

```bash
make harness-up      # build images, start two Samba servers + the dev container
make verify-cifs     # prove this kernel can mount CIFS at all — run this first
make demo            # walk the Phase 1 exit criteria with curl, printing every response
```

`make verify-cifs` is the one that matters before anything else. It mounts a real share inside the
container and prints `CIFS OK`. If it fails with an unknown-filesystem error, the Docker VM's kernel
has no `cifs` module and no amount of code will help — you need a Linux host or a different VM.

`make demo` starts the server, then exercises: adding a target by IP, testing it, a guest share,
a wrong password, a nonexistent share, an unreachable host, mount cleanup, and credential hygiene.
It prints the full JSON response for each so the error messages can be read and judged.

## Running the whole verification suite

```bash
make harness-up && make verify-cifs && make test-unit && make lint && make test-integration && make demo
```

Roughly five minutes from cold. All of it must pass before a phase counts as done (CLAUDE.md).

## Commands

Run `make help` for this list at any time.

| Command | What it does |
|---|---|
| `make harness-up` | Build and start the Samba servers + dev container, then wait for port 445 |
| `make harness-down` | **Stop the harness and delete its volumes** |
| `make verify-cifs` | Prove the host kernel supports CIFS mounts |
| `make demo` | Walk the Phase 1 exit criteria with curl |
| `make test-unit` | Unit tests under the race detector — no kernel or Samba needed |
| `make test-integration` | Integration tests against the Samba harness, under `-race` |
| `make test` | Both test suites |
| `make lint` | `golangci-lint run` |
| `make build` | Compile the server binary inside the container |
| `make tidy` | `go mod tidy` |
| `make dev-shell` | Interactive shell in the dev container |

### Tearing down

```bash
make harness-down    # stops 3 containers and removes the Go module/build cache volumes
```

`harness-down` passes `-v`, so the cached Go modules and build cache go with it — the next
`harness-up` re-downloads them. To stop the containers but keep the caches:

```bash
docker compose -f docker-compose.test.yml down
```

To check what is still running:

```bash
docker compose -f docker-compose.test.yml ps
```

## Poking at the API by hand

The dev container publishes no ports, so the server is not reachable from your machine. Work from
inside it:

```bash
make dev-shell

# in the container:
go build -o /tmp/smbsync ./cmd/smbsync
ENCRYPTION_KEY=some-long-development-key DATA_DIR=/tmp/data MOUNT_ROOT=/mnt/smb /tmp/smbsync &

# add a target by IP (172.28.0.10 is the first Samba server)
curl -sS -X POST localhost:8384/api/targets -H 'Content-Type: application/json' -d '{
  "name": "nas-a", "type": "smb", "host": "172.28.0.10",
  "share": "private", "username": "syncuser", "password": "syncpass"
}' | jq .

# mount it, statfs it, list its root
curl -sS -X POST localhost:8384/api/targets/<id>/test | jq .
```

Phase 1 endpoints: `POST|GET /api/targets`, `GET|PATCH|DELETE /api/targets/{id}`,
`POST /api/targets/{id}/test`, `GET /healthz`. Leave `username` empty to mount as a guest.

## Configuration

| Variable | Default | Notes |
|---|---|---|
| `ENCRYPTION_KEY` | — | **Required.** Encrypts stored credentials. Minimum 16 characters; startup fails without it. Changing it makes existing credentials unreadable |
| `LISTEN_ADDR` | `:8384` | |
| `DATA_DIR` | `/data` | SQLite database lives here |
| `MOUNT_ROOT` | `/mnt/smb` | Shares are mounted at `<MOUNT_ROOT>/<target-id>` |
| `MOUNT_UID` / `MOUNT_GID` | process uid/gid | Becomes `uid=`/`gid=` in the mount options |
| `MOUNT_TIMEOUT` | `30s` | Per mount attempt, per dialect |
| `STATFS_TIMEOUT` | `5s` | Stale-mount watchdog |
| `UNMOUNT_TIMEOUT` | `15s` | |
| `MOUNT_IDLE_GRACE` | `60s` | How long an unreferenced mount survives, so back-to-back jobs reuse it |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## Why the container is privileged

This is deliberate, and worth understanding before deploying it (SPEC.md §3):

- **`CAP_SYS_ADMIN` and `CAP_DAC_READ_SEARCH`** — `mount.cifs` cannot mount anything without them.
  A container with `SYS_ADMIN` is close to root on the host. Run this on a machine you trust.
- **`apparmor:unconfined`** — some hosts block mounting from a container regardless of capabilities.
- **`network_mode: host`** (production) — removes NAT overhead between the container and the SMB
  server, which matters because saturating a gigabit link is a first-class requirement.
- **Let the container own its mounts.** If the host already mounts the same share, the kernel may
  share the CIFS superblock and mix mount options between them (SPEC.md §13).

Credentials are never passed on a command line — `mount.cifs` receives them through a `0600` file
that is deleted immediately after the mount, so they never appear in `ps` or in logs.

The test harness deviates in one way: it uses a bridge network with static IPs rather than host
networking, because host networking does not behave on Docker Desktop for macOS. Targets are still
addressed by IP.

## Repository layout

```
cmd/smbsync/       server entrypoint
internal/
  api/             HTTP handlers
  config/          environment configuration
  health/          cached per-target reachability
  mountmgr/        CIFS mount manager: dialect ladder, refcounting, reaper, error mapping
  secrets/         credential encryption at rest
  storage/         protocol-agnostic Storage abstraction (SPEC.md §4)
  store/           SQLite schema, migrations, target CRUD
test/              integration tests (build tag: integration)
  samba/           the Samba server image used by the harness
```

## Troubleshooting

**Docker commands hang and print nothing.** The daemon is wedged — this happened during development.
Confirm with `curl --max-time 5 --unix-socket ~/.docker/run/docker.sock http://localhost/_ping`; if
it never answers, restart Docker Desktop.

**`make verify-cifs` fails with an unknown filesystem type.** The VM kernel lacks the `cifs` module.
Nothing in this project can work around that; use a Linux host.

**Integration tests report an unreachable host.** The Samba containers may not have bound port 445
yet. `make harness-up` waits for them; if you started the stack with `docker compose` directly, run
`docker compose -f docker-compose.test.yml exec dev bash /src/test/wait-for-samba.sh`.

**`go mod tidy` complains about the toolchain version.** The dev image pins Go 1.25 because
`modernc.org/sqlite` requires it. Rebuild the image: `make harness-up`.
