# cn4m-cascade — SMB Sync Engine

A self-hosted file sync tool for SMB shares and local folders, run from a web UI and packaged as a single
container. Sources and destinations are primarily **SMB/CIFS shares addressed by IP**, mounted on
demand by the kernel and treated as ordinary filesystem paths, so the sync engine itself never
learns about SMB.

**[SPEC.md](SPEC.md) is the source of truth** for design and scope. [CLAUDE.md](CLAUDE.md) holds the
working rules; [PROGRESS.md](PROGRESS.md) tracks what is built and what is next.

## Status

Built in phases (SPEC.md §11). **Phases 1 to 5 are complete, and Phase 6 has begun with the
production packaging** — so this is deployable: `make image && make run`, then open
<http://localhost:2649>.

| | |
|---|---|
| ✅ Works today | The full web UI — dashboard, jobs, targets, run detail, logs, settings. Mirror and update modes, fan-out to many destinations, include/exclude filters with global exclusions, on-demand kernel CIFS mounts with refcounting and an idle grace period, SMB dialect and multichannel fallback, encrypted credentials, preview-before-run, cron scheduling, webhook triggers and signed outbound callbacks, cn4m suite reporting, and a production Docker image |
| ⛔ Not built yet | Bandwidth limiting, throughput graph, log retention, portable configuration export |
| 🚫 Not planned | **Two-way sync** — deferred indefinitely, SPEC.md §14.1 |

Tested at 100,000 files.

> ### ⚠️ Where you deploy this decides its speed
>
> **A container on Docker Desktop moved 145 MB/s where the host moved 771 MB/s to the same NAS over
> the same 10GbE link.** Containers there run inside a VM behind an internal gateway, and even
> `--network host` does not escape it — so SPEC.md §3.3's `network_mode: host`, which exists "to
> eliminate NAT overhead", cannot do its job on Docker Desktop for Windows or macOS.
>
> **For throughput-sensitive syncing, deploy on native Linux with `network_mode: host`**, or run
> the binary directly on the host. Docker Desktop is fine for development and for small or
> latency-tolerant jobs; it costs roughly 5× on sustained transfers.
>
> **On Windows there is now a native build** (SPEC.md §11, Phase 6b): a single `.exe`, no Docker, no
> `mount.cifs` — it authenticates UNC paths through the Windows network redirector instead. Measured
> at **~434 MB/s against 145 MB/s** for the same job in a container on the same host. Credentials
> never touch disk on that path.
>
> Full measurements, the diagnosis, and how to run the native build are in
> **[PERFORMANCE.md](PERFORMANCE.md)**.

## Sync modes

| Mode | What it does |
|---|---|
| **Update** | Copies new and newer files only. **Never deletes anything at the destination**, whatever is there. The default for a new job. |
| **Mirror** | Makes the destination match the source. Copies new and changed files, **and deletes destination files the source no longer has.** |

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

### Picking files from a JSON catalogue

A filter rule can take its patterns from a JSON file (**source: JSON file**, plus a key). The key is
a dot-path; numeric segments index arrays; and **`*` matches every value of an object or every
element of an array**. That last part is what makes a catalogue keyed by ids you cannot predict
usable:

```json
{
  "tracked_flags": {},
  "tracked_repo_assets": {
    "cb2cf6dbd5ecbcd83ac9aab1e4a85c45": { "name": "1205_A1_EvanOpening_v001.mov", "size": "594.6 MiB" },
    "7ee451f5837d8174bea08f2b1cb7c86b": { "name": "1519_A1_RDJWalkOn_v000.mov",  "size": "2.475 GiB" }
  },
  "untracked_repo_assets": {
    "aa11bb22cc33dd44ee55ff6677889900": { "name": "2001_B2_Finale_v003.mov" }
  }
}
```

**The key field takes one key per line**, and the results are combined, so both sections come from a
single rule:

```
tracked_repo_assets.*.name
untracked_repo_assets.*.name
```

Set the rule's direction to **include** and only those files are synced. Three things worth knowing:

- **Use the filenames, not the folders.** Patterns match at any depth, so `1205_A1_EvanOpening_v001.mov`
  is found wherever it lives. A catalogue's `folder` field is a path on the origin server, which is
  usually not the path under your sync root — you do not need to translate it.
- **A `*` key is a query; a plain key is an assertion.** With a wildcard, a section that is missing
  or empty, an entry without the field, and a non-string value are all skipped, because a catalogue's
  shape varies legitimately. Without one, a key that does not resolve is an error — so a typo in
  `backup.exclude` still fails loudly instead of quietly selecting nothing.
- **A rule that ends up matching nothing is reported** in the run log (a warning, or an error for a
  global rule). Worth checking after the first run: an include rule that resolves to zero patterns
  copies nothing and still finishes green.

The file is re-read at the start of every run, so regenerating the catalogue is enough — you do not
need to re-save the job. It can live on a share (`target://<target-id>/path/catalogue.json`) or on a
bind-mounted local path.

## Requirements

- **Docker** (Docker Desktop is fine). The daemon must be responsive — see [Troubleshooting](#troubleshooting).
- **Nothing else.** You do not need Go, `cifs-utils`, or a Samba install on your machine. Every
  build, lint and test runs inside the dev container, because `mount.cifs` is Linux-only and the
  host is often macOS. Every `make` target is a thin `docker compose` wrapper.

## Quickstart (Docker)

The fastest path from a clone to a working UI. You need **Docker and nothing else** — no Go, no
Node, and no `make`, which matters on Windows where `make` is not standard.

**1. Create `.env`.** The server refuses to start without an encryption key, because one that came
up without a key could not store a credential. The template ships a **placeholder** that is long
enough to start the server, so replacing it is on you — a forgotten one is a real key everyone with
the repository knows.

```bash
cp .env.example .env
openssl rand -base64 32          # REPLACE the ENCRYPTION_KEY placeholder with this
```

On Windows PowerShell, where there is no `openssl`:

```powershell
Copy-Item .env.example .env
$b = New-Object byte[] 32
[Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
[Convert]::ToBase64String($b)    # REPLACE the ENCRYPTION_KEY placeholder with this
```

**2. Build and start it.** One command builds the image and brings the stack up:

```bash
docker compose -f docker-compose.yml up -d --build
```

**3. Open <http://localhost:2649>** and set an admin password. There is no default account; the
first visit creates one, and first-run setup then closes.

**4. Add a target** and press **Save and test**. For a NAS, that is the host, share and credentials.

> **Local folders in Docker are `/mnt/local`, and this is the one thing that trips people up.** A
> container can only see what is mapped into it, so the path you type is the path *inside* the
> container — never the path on your own machine. `/mnt/local` is the folder `docker-compose.yml`
> maps there, `%USERPROFILE%\cn4m` or `~/cn4m` by default, and subfolders like
> `/mnt/local/photos` work. `/Users/you/cn4m` means nothing to the container even though it exists on
> your machine.
>
> **This applies to Docker only.** Run the binary natively and there is no container and no mapping:
> a local target is just a real path, `M:\projects\repo` or `/srv/media`. See
> [Quickstart (native Windows)](#quickstart-native-windows).
>
> To map a second folder, see [Adding another local folder](#adding-another-local-folder-docker).

**Then:**

```bash
docker compose -f docker-compose.yml logs -f     # follow it
docker compose -f docker-compose.yml down        # stop it
docker compose -f docker-compose.yml up -d       # start it again
```

With `make` available, `make image`, `make run`, `make logs` and `make down` are wrappers for the
same things. Read [Deploying it for real](#deploying-it-for-real) before you rely on it — the
backup rule there is the part that bites.

**Back up `./data` and your `ENCRYPTION_KEY` together.** The database holds encrypted credentials
and the key decrypts them; either alone is useless and there is no recovery from losing the key.

## Quickstart (native Windows)

No Docker, no `mount.cifs` — one `.exe`. This is the **recommended way to run it on Windows**, because
a container there costs roughly 5× on sustained transfer ([PERFORMANCE.md](PERFORMANCE.md)).

**1. Build it** (or use a release binary):

```bash
make windows-exe          # cross-compiles into ./dist from the dev container
```

**2. Configure it.** The binary reads a **`.env` file sitting beside the executable** — the same
format as the compose deployment, so there is only one configuration language to know:

```powershell
copy dist\.env.example dist\.env
notepad dist\.env
```

Set `ENCRYPTION_KEY` to a long random value. Generate one with:

```powershell
$b = New-Object byte[] 32
[Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
[Convert]::ToBase64String($b)
```

A minimal `dist\.env`:

```ini
ENCRYPTION_KEY=<paste the value>
CN4M_CASCADE_STATUS_URL=off      # omit if this install reports to a cn4m suite
```

Everything else has a working default. **Environment variables override the file**, the same
precedence `docker compose` gives its `.env`, so `$env:LOG_LEVEL="debug"; .\cn4m-cascade.exe` works
for a one-off without editing anything.

Notepad's defaults are fine — a UTF-8 byte-order mark and CRLF line endings are both handled.

> ⚠️ **The key must stay the same forever.** It decrypts the stored target passwords. Change it and
> every SMB target goes unhealthy until you re-enter its password; there is no recovery. **Back up
> `.env` together with your database.**

Prefer environment variables to a file? `setx ENCRYPTION_KEY "<value>"` works too — then open a new
PowerShell, since `setx` does not affect the current session.

**3. Run it:**

```powershell
.\dist\cn4m-cascade.exe
```

It logs which configuration file it read, so there is no doubt about which copy is in effect:

```json
{"level":"INFO","msg":"configuration file loaded","path":"...\dist\.env"}
```

**4. Open <http://localhost:2649>**, set an admin password, and add a target. For a NAS that is the
host, share and credentials. For a folder on this machine it is an ordinary Windows path —
`M:\projects\repo`, not `/mnt/local`; there is no container to map anything into.

To have it start at boot instead, see **Running it as a service** in
[PERFORMANCE.md](PERFORMANCE.md#running-it-as-a-service). The service reads the same `.env`.

### Configuration

Everything is an environment variable. Only the first is required.

| Variable | Default | Notes |
|---|---|---|
| `ENCRYPTION_KEY` | *(none — refuses to start)* | At least 16 characters. Never changes. Back it up. |
| `CN4M_CASCADE_CONFIG` | *(none)* | An explicit config file path, instead of the `.env` beside the executable. A path that cannot be read is an error, not a warning. |
| `DATA_DIR` | `%ProgramData%\cn4m-cascade` | Holds the SQLite database. Works without elevation, and a service and an interactive operator see the same one. |
| `LISTEN_ADDR` | `:2649` | `127.0.0.1:2649` binds to loopback only. |
| `CN4M_CASCADE_ADMIN_PASSWORD` | *(none)* | Sets the admin password on a fresh database instead of doing it in the browser. |
| `CN4M_CASCADE_STATUS_URL` | cn4m on `localhost:2640` | `off` if this install is not part of a cn4m suite. |
| `TZ` | the machine's zone | Cron schedules are read in this zone. |
| `LOG_LEVEL` | `info` | `debug` for more. |

`MOUNT_ROOT`, `MOUNT_UID` and `MOUNT_GID` are Linux-only and ignored: Windows mounts nothing, it
authenticates UNC paths directly.

Any of these may go in the `.env` file or the environment. The file is looked for **beside the
executable only** — not the working directory, because a Windows service runs with its working
directory in `system32` and a cwd-relative search would find nothing in the deployment that needs a
config file most.

**Back up `DATA_DIR` and `ENCRYPTION_KEY` together.** The database holds encrypted credentials and the
key decrypts them; either alone is useless and there is no recovery from losing the key.

## Quickstart (development harness)

This one builds from source in the *dev* container and is for working on the code, not for running
it. To just run it, use the Docker quickstart above.

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

## Deploying it for real

The quickstart above runs everything from source inside the *development* container. That is not how
you deploy it. Production is a separate ~24 MB image with no Go toolchain, no Node, and no test
harness in it.

```bash
make image      # build cn4m-cascade:latest
make run        # start it on http://localhost:2649
make logs       # follow it
make down       # stop it
```

`make run` needs a `.env` beside the compose file and refuses to start without one, because a server
that comes up without an encryption key would be a server that cannot store a credential. Start from
the template:

```bash
cp .env.example .env
openssl rand -base64 32        # paste into ENCRYPTION_KEY
```

`.env.example` documents every setting; only `ENCRYPTION_KEY` is required. `make run` also prints a
generated key if you try it without a `.env`.

**Back up `./data` and `ENCRYPTION_KEY` together.** The database holds the encrypted credentials and
the key decrypts them: either one alone is useless, and there is no recovery path from losing the
key. Both are gitignored, so neither is in the repository — a fresh clone starts with no targets and
no jobs, which is correct for a separate installation and worth knowing if you expected otherwise.

The production stack and the test harness are deliberately separate compose projects on different
ports (2649 and 12649), so `make run` and `make test-integration` cannot interfere. They used to
share a container, which twice ended in a wedged Docker daemon.

## Windows notes

Windows runs a **WSL2** kernel rather than the LinuxKit one Docker Desktop uses on a Mac, and
`mount.cifs` needs kernel CIFS support. That was verified on 2026-09-09 — Windows 11 Pro 26200,
Docker Desktop 26.1.4, kernel `5.15.153.1-microsoft-standard-WSL2` — end to end: a real NAS mounted
from the production image, saved through the UI, and a job copied a tree. Nothing here is
Windows-specific in the code; what follows is the environment.

- **Line endings will break the harness, and the lint gate, before anything else does.**
  `.gitattributes` pins the whole tree to LF (`* text=auto eol=lf`). If you cloned before that pin,
  or Git has `core.autocrlf=true` and files came out CRLF, the Samba containers exit 1 with
  `exec /usr/local/bin/entrypoint.sh: no such file or directory` — the file is there, but its shebang
  ends in a carriage return and the kernel looks for an interpreter named `/bin/sh<CR>`. The same
  CRLF makes `gofmt` treat every Go file as unformatted, so `golangci-lint run` fails on files nobody
  has touched. Fix a stale checkout with `git add --renormalize . && git checkout -- .` — commit or
  stash first, because that second command overwrites the worktree. Neither symptom points at line
  endings, and neither can happen on macOS or Linux.
- **In Docker, paths in `.env` use forward slashes**: `CN4M_CASCADE_LOCAL_DIR=C:/Users/you/cn4m`.
  Docker Desktop accepts them; backslashes are mangled by compose interpolation. The folder appears
  inside the container as `/mnt/local`, which is what you type into the UI there — see **Adding
  another local folder (Docker)** below. **None of this applies to the native build**, which takes
  ordinary Windows paths like `M:\projects\repo`.
- `make` is not standard on Windows. Use Git Bash or WSL, or run the underlying commands directly:
  `docker build -t cn4m-cascade:latest .` and `docker compose -f docker-compose.yml up -d`.
- **A LAN NAS is reached through WSL2 NAT**, a layer Docker Desktop on a Mac does not have. A
  `mount error(113)` or a hang on an address that answers fine from PowerShell is a networking
  problem, not a CIFS one.

## Adding another local folder (Docker)

**Docker only.** A native install needs none of this: with no container there is nothing to map, and a
local target is given a real path directly.

The container sees exactly the host folders `docker-compose.yml` maps into it. `/mnt/local` is set up
for you; anything else needs a bind mount, and the path you type into the UI is the path *inside* the
container, never the host path.

To point the existing mount somewhere else, set it in `.env`:

```ini
# Windows — forward slashes, and quote nothing
CN4M_CASCADE_LOCAL_DIR=D:/Media/Projects
```

That still appears as `/mnt/local`. To have **more than one**, add a volume to the `cn4m-cascade` service
in `docker-compose.yml` — there is a commented example there to copy:

```yaml
    volumes:
      - ./data:/data
      - ${CN4M_CASCADE_LOCAL_DIR:-${HOME:-${USERPROFILE:-.}}/cn4m}:/mnt/local:rw
      # A second folder, read-only so it can only ever be a source:
      - D:/Media/Footage:/mnt/footage:ro
```

Then create a local target whose path is `/mnt/footage`. Mounting a source `:ro` is worth doing: it
makes the kernel refuse a write to it, whatever a job is later configured to do.

Compose only reads the volume list at container creation, so `docker compose -f docker-compose.yml up
-d` after editing it — a restart is not enough.

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

## Notifications

A job can push its status to outbound webhooks (SPEC.md §8.2), configured per job or globally in the
**Webhooks & API** tab. Three wire formats:

| Format | For | Signed | On failure |
|---|---|---|---|
| `json` | n8n, Home Assistant, anything custom | HMAC-SHA256 | logged at warn |
| `cn4m` | the cn4m suite status view | no | silent |
| `discord` | a Discord channel | no | silent |

`cn4m` and `discord` are **best-effort on purpose**: a suite dashboard or a chat service being
unreachable is somebody else's outage, not a fault in your backup, so a failed delivery never appears
in the run log and never fails a sync. `json` keeps its warn-level logging, because a custom
integration that quietly stops arriving *is* worth seeing.

### Discord

Create a webhook in Discord (**Server Settings → Integrations → Webhooks**) and either paste its URL
into the Webhooks & API tab with format **Discord**, or set it in your `.env` to have it created
automatically on a fresh database:

```ini
CN4M_CASCADE_DISCORD_WEBHOOK=https://discord.com/api/webhooks/<id>/<token>
```

You get one line per finished run:

```
🔄 ✅ Sync complete — 6 succeeded, 0 failed, 1 skipped
🔄 ⚠️ Sync incomplete — 1 succeeded, 0 failed, 1 skipped
🔄 ❌ Sync failed — 0 succeeded, 1 failed, 0 skipped
```

Deliberately just the tally. The destination id, the paths and the full error stay in the run log —
a chat line is read at a glance, and the message is there to tell you to go and look.

Subscribe it to **`run_completed` and `run_failed` only**. `progress` posts a line every
`min_interval_sec` for the length of every run, which is noise in a channel people read.

> **The webhook URL is a credential.** Anyone holding it can post to that channel. Keep it in `.env`,
> which is gitignored; it is stored encrypted like any other webhook secret and is never written to a
> log. The environment variable seeds a **fresh** database only and never overwrites, so editing the
> row in the UI is permanent.

## Poking at the API by hand

The dev container publishes 12649 (the server) and 5173 (the Vite dev server), but the simplest way
to drive the API by hand is from inside it:

```bash
make dev-shell

# in the container:
go build -o /tmp/cascade ./cmd/cn4m-cascade
ENCRYPTION_KEY=some-long-development-key DATA_DIR=/tmp/data MOUNT_ROOT=/mnt/smb /tmp/cascade &

# add a target by IP (172.28.0.10 is the first Samba server)
curl -sS -X POST localhost:2649/api/targets -H 'Content-Type: application/json' -d '{
  "name": "nas-a", "type": "smb", "host": "172.28.0.10",
  "share": "private", "username": "syncuser", "password": "syncpass"
}' | jq .

# mount it, statfs it, list its root
curl -sS -X POST localhost:2649/api/targets/<id>/test | jq .
```

Endpoints so far:

```
POST|GET       /api/targets                 GET|PATCH|DELETE /api/targets/{id}
POST           /api/targets/{id}/test
POST           /api/targets/{id}/mkdir      → creates one folder under a target

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
curl -sX POST localhost:2649/api/auth/setup \
  -H 'content-type: application/json' \
  -d '{"password":"a good long password"}' -c cookies.txt
```

…or seed it from the environment, which is what a compose file should do:

```yaml
environment:
  CN4M_CASCADE_ADMIN_PASSWORD: "a good long password"
```

`CN4M_CASCADE_ADMIN_PASSWORD` only ever *sets* an unset password — it never overwrites an existing one,
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

One asymmetry worth knowing: an **empty** `CN4M_CASCADE_ADMIN_PASSWORD` means "not configured", not "no
password". SPEC.md §10's compose file passes `ADMIN_PASSWORD=${ADMIN_PASSWORD}`, which expands to an
empty string whenever the variable is unset on the host — treating that as a deliberate blank would
turn a forgotten variable into a server anyone can sign into. Choosing no password has to be an
explicit act, so it is only available through the first-run setup form (or an empty `password` in
the `/api/auth/setup` body).

Then sign in and keep the cookie:

```bash
curl -sX POST localhost:2649/api/auth/login \
  -H 'content-type: application/json' \
  -d '{"password":"a good long password"}' -c cookies.txt

curl -s localhost:2649/api/targets -b cookies.txt
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
curl -sX POST localhost:2649/api/jobs/$JOB/run -b cookies.txt \
  -H 'content-type: application/json' -d '{"preview":true}'

# Inspect what it intends to do
curl -s localhost:2649/api/runs/$RUN -b cookies.txt | jq '.progress.plans'

# Go ahead
curl -sX POST localhost:2649/api/jobs/$JOB/confirm -b cookies.txt
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

## Syncing a folder from your own machine (Docker)

**Docker only.** Running natively there is nothing to share: the server reads the machine's own
filesystem, so a local target takes an ordinary path — `M:\projects\repo`, `/srv/media` — and none
of the mapping below applies.

In a container the server can only read paths that exist *inside* it. One folder on your machine is
shared in by default:

| Your machine | Inside the container |
|---|---|
| `~/cn4m` (macOS, Linux) | `/mnt/local` |
| `%USERPROFILE%\cn4m` (Windows) | `/mnt/local` |

`make harness-up` creates it. To use it, add a target of type **Local folder** with the path
`/mnt/local` — or a subfolder such as `/mnt/local/photos`. That target can be a source or a
destination like any other.

Type the *container* path (`/mnt/local`), not the path on your own machine. `/Users/you/cn4m` means
nothing inside the container and the target will not resolve.

To share a different folder instead, set `CN4M_CASCADE_LOCAL_DIR` — in your shell, or in a `.env` file beside
`docker-compose.test.yml`:

```
CN4M_CASCADE_LOCAL_DIR=/Volumes/media/to-back-up
```

Then `make harness-up` again. It still appears as `/mnt/local` inside the container, so nothing you
configured has to change. On Linux, files the server writes there are owned by root, because the
container runs as root; `sudo chown -R "$USER" ~/cn4m` if that gets in your way.

## When a destination is unreachable

`unavailable_policy` decides what happens when a destination cannot be reached:

| Value | Behaviour |
|---|---|
| `skip` (default) | Log it, mark that destination skipped, carry on. The run ends `partial`. |
| `abort` | Fail the whole run immediately. |
| `prompt` | Park **that destination** and wait for a person. The run keeps going, and its other destinations finish normally. |

Answer a prompt with:

```bash
curl -sX POST localhost:2649/api/runs/$RUN/prompt -b cookies.txt \
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
| `LISTEN_ADDR` | `:2649` | |
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
- **Bridge networking with a published port** (production) — `network_mode: host` was the original
  plan and is Linux-only: on Docker Desktop the container lives in a VM, so "host" means the VM and
  the UI is unreachable from your browser. On a Linux host you may swap it back to remove NAT
  overhead between the container and the SMB
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
cmd/cn4m-cascade/  server entrypoint
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

**A target will not mount.** The message names the layer that failed:

| Message | Meaning |
|---|---|
| `mount error(2): No such file or directory` | the share name is wrong |
| `mount error(13): Permission denied` | credentials, or the server requires a dialect this did not offer |
| `mount error(113): could not connect` | the host is unreachable — routing or firewall, not CIFS |
| `wrong fs type, bad option, bad superblock` | the kernel has no CIFS support; nothing here can work around it |
| `Operation not permitted` | the capabilities did not apply; check `SYS_ADMIN` survived your shell quoting |

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
