# cn4m-cascade — SMB Sync Engine

A self-hosted sync tool in the spirit of FreeFileSync, run from a web UI and packaged as a single
container. Sources and destinations are primarily **SMB/CIFS shares addressed by IP**, mounted on
demand by the kernel and treated as ordinary filesystem paths, so the sync engine itself never
learns about SMB.

**[SPEC.md](SPEC.md) is the source of truth** for design and scope. [CLAUDE.md](CLAUDE.md) holds the
working rules; [PROGRESS.md](PROGRESS.md) tracks what is built and what is next.

## Status

Built in phases (SPEC.md §11). **Phases 1 and 2 of 6 are complete.**

| | |
|---|---|
| ✅ Works today | Target CRUD over HTTP; on-demand kernel CIFS mounts with refcounting and an idle grace period; SMB dialect and multichannel fallback; stale-mount detection; encrypted credentials; **the sync engine — mirror and update modes to a single destination, with a concurrent scanner, parallel copies, resumable temp-file writes, retries, cancellation, a task log and live progress/ETA** |
| ⛔ Not built yet | Filters, multi-destination fan-out, the web UI, scheduling, webhooks, two-way sync, rename/move detection |

There is no UI yet — everything is driven over HTTP. Syncing works: a job mirrors or updates one
source to one destination, and has been tested at 100,000 files.

## Sync modes

| Mode | What it does |
|---|---|
| **Mirror** | Makes the destination match the source. Copies new and changed files, **and deletes destination files the source no longer has.** |
| **Update** | Copies new and newer files only. **Never deletes anything at the destination**, whatever is there. |

Mirror is the one that removes data, so it has several guards:

- **Every deletion is announced before it happens** — a warning naming the count and total bytes,
  then one log line per file and per directory removed. Deletion logging is never summarised away
  and cannot be turned off.
- **Deletions are withheld when the source listing might be wrong.** If any source directory could
  not be read, or the source scans as completely empty while the destination is not (a dropped
  mount, or a stale cached listing), nothing is deleted and the run finishes `partial` with the
  reason recorded.
- **Deletions are withheld when copies failed**, unless the job sets `delete_policy: proceed`.
  Extra files at the destination are corrected by the next clean run; a wrong deletion is not.
- **An incomplete source scan blocks deletions unconditionally** — that one is not a policy and
  cannot be overridden.

Update never deletes under any circumstance, including when the source has a directory where the
destination has a file. Such conflicts are reported and skipped rather than resolved.

### Filters and deletion

A path excluded by a filter is **out of scope on both sides**: it is never copied, never deleted,
and never considered by the comparison.

This matters more than it looks. Excluding `cache/` means the source's `cache/` is not copied — it
does **not** mean the destination's existing `cache/` is now "missing from the source" and due for
deletion. Adding an exclude rule to save bandwidth must never destroy what is already backed up. If
you want a directory removed from the destination, delete it at the source and let mirror propagate
that, or remove it by hand.

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
| `make harness-clean` | Clear leftover mounts and firewall rules from a test run that was killed |
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

Endpoints so far:

```
POST|GET       /api/targets                 GET|PATCH|DELETE /api/targets/{id}
POST           /api/targets/{id}/test

POST|GET       /api/jobs                    GET|DELETE       /api/jobs/{id}
POST           /api/jobs/{id}/run           → 202, runs in the background

GET            /api/runs                    GET              /api/runs/{id}
GET            /api/runs/{id}/events        POST             /api/runs/{id}/cancel

GET            /healthz
```

Leave `username` empty to mount as a guest. `GET /api/runs/{id}` includes live progress and ETA
while a run is in flight.

## Signing in

Every `/api/*` route needs a session, except `/api/auth/*` and `/healthz`.

On a fresh database there is no password yet. Either set one at first run:

```bash
curl -sX POST localhost:8384/api/auth/setup \
  -H 'content-type: application/json' \
  -d '{"password":"a good long password"}' -c cookies.txt
```

…or seed it from the environment, which is what a compose file should do:

```yaml
environment:
  SMBSYNC_ADMIN_PASSWORD: "a good long password"
```

`SMBSYNC_ADMIN_PASSWORD` only ever *sets* an unset password — it never overwrites an existing one,
so leaving it in a compose file cannot silently reset the credential on every restart.

### There is no password policy

Any password is accepted: one character, two, or **none at all**. This is deliberate. The service
is designed to run on a closed network alongside the NAS boxes it syncs, where a long password is
friction on every sign-in and protects against nobody who is not already inside the network.

Understand what a blank password means before choosing it: **anyone who can reach this port can
sign in.** There is no second factor and no lockout that helps, because there is nothing to guess.
That is fine on a segregated VLAN or a home LAN behind a router. It is not fine if the machine has
a public interface, sits on shared office Wi-Fi, or is reachable through a VPN that other people
also use. **If the network is open, or you are unsure, set a real password** — the storage is the
same either way (a fresh salt and 600k PBKDF2-HMAC-SHA256 iterations), so a strong password costs
nothing but typing it.

Two things stay true no matter how short the password is:

- **A blank password is still a credential, not a disabled check.** `POST /api/auth/login` with the
  wrong password is still a 401, and the rate limiter still applies. It is not an "auth off" switch.
- **First-run setup still closes after the first use**, so nobody else can claim a configured
  instance by racing you to `/api/auth/setup`.

One asymmetry worth knowing: an **empty** `SMBSYNC_ADMIN_PASSWORD` means "not configured", not "no
password". SPEC.md §10's compose file passes `ADMIN_PASSWORD=${ADMIN_PASSWORD}`, which expands to an
empty string whenever the variable is unset on the host — treating that as a deliberate blank would
turn a forgotten variable into a server anyone can sign into. Choosing no password has to be an
explicit act, so it is only available through the first-run setup form (or an empty `password` in
the `/api/auth/setup` body).

Then sign in and keep the cookie:

```bash
curl -sX POST localhost:8384/api/auth/login \
  -H 'content-type: application/json' \
  -d '{"password":"a good long password"}' -c cookies.txt

curl -s localhost:8384/api/targets -b cookies.txt
```

`GET /api/auth/session` reports `{"setup_required":true}` before first-run setup, which is how the
UI decides whether to show a setup form or a login form.

The cookie is `HttpOnly` and `SameSite=Lax`. It is marked `Secure` **only** when the request
arrived over TLS: this service is normally reached over plain HTTP on a LAN, and an unconditional
`Secure` flag would make the browser throw the cookie away and login would fail with nothing
visible to explain it. Put it behind a TLS proxy and the flag turns itself on.

## Previewing a run before it happens

```bash
# Plan the work and hold it — nothing is copied or deleted
curl -sX POST localhost:8384/api/jobs/$JOB/run -b cookies.txt \
  -H 'content-type: application/json' -d '{"preview":true}'

# Inspect what it intends to do
curl -s localhost:8384/api/runs/$RUN -b cookies.txt | jq '.progress.plans'

# Go ahead
curl -sX POST localhost:8384/api/jobs/$JOB/confirm -b cookies.txt
```

A previewed run holds its mounts while it waits. If nobody confirms within the job's
`prompt_timeout_sec` (default 600), it **cancels itself and changes nothing** — an unconfirmed plan
must never execute.

Two things to know about the hold:

- **A confirmed preview executes the plan it showed you, not a freshly computed one.** That is
  deliberate: confirming a plan should carry out the plan you approved. But it means the plan can be
  stale by however long you took to confirm. If something else writes to the destination in that
  window, a `delete` in the plan still applies to that path. Keep `prompt_timeout_sec` short on
  mirror jobs, where the plan can contain deletions.
- **The job cannot be edited while one of its runs is parked.** A `PATCH /api/jobs/{id}` returns
  `409 job_running`. Editing the job would not change the plan already being held, so an edit that
  looked like it had taken effect would not have — adding an exclude rule and then confirming would
  still run the old plan.

The preview gate is the one thing that holds an entire run. An unreachable destination under
`prompt` does not — see below.

## When a destination is unreachable

`unavailable_policy` decides what happens when a destination cannot be reached:

| Value | Behaviour |
|---|---|
| `skip` (default) | Log it, mark that destination skipped, carry on. The run ends `partial`. |
| `abort` | Fail the whole run immediately. |
| `prompt` | Park **that destination** and wait for a person. The run keeps going, and its other destinations finish normally. |

Answer a prompt with:

```bash
curl -sX POST localhost:8384/api/runs/$RUN/prompt -b cookies.txt \
  -H 'content-type: application/json' \
  -d '{"dest_target_id":"'$DEST'","action":"skip"}'   # skip | retry | abort
```

If nobody answers within `prompt_timeout_sec`, the run falls back to `prompt_fallback` (default
`skip`) and records that it did so in the run log — a destination is never quietly dropped. The
wait is always bounded, because a run may be started by a schedule with nobody watching.

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

**Integration tests fail with `mount error(115): Operation now in progress`.** Almost always a dirty
harness rather than a real failure. A test process killed before its cleanup ran leaves behind an
`iptables` blackhole (from the cable-pull test) and CIFS mounts that retry forever; while the kernel
is mid-reconnect to a server, a *new* mount to that server returns 115. Run `make harness-clean` —
`make test-integration` now does it for you. The tell is that only the first test or two fail, and
the same shares work later in the run.

**Integration tests report an unreachable host.** The Samba containers may not have bound port 445
yet. `make harness-up` waits for them; if you started the stack with `docker compose` directly, run
`docker compose -f docker-compose.test.yml exec dev bash /src/test/wait-for-samba.sh`.

**`go mod tidy` complains about the toolchain version.** The dev image pins Go 1.25 because
`modernc.org/sqlite` requires it. Rebuild the image: `make harness-up`.
