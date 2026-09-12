# Performance

Where throughput actually goes, measured rather than reasoned about. Every number here came from a
benchmark on a real deployment (Windows 11 Pro 26200, Docker Desktop 26.1.4, WSL2 kernel
`5.15.153.1-microsoft-standard-WSL2`) and is reproducible with the commands at the end.

> ## ⚠️ Do not run this on Docker Desktop if throughput matters
>
> **Measured on this project: a container on Docker Desktop for Windows moved 145 MB/s where the
> host moved 771 MB/s to the same destination over the same 10GbE link — a 5× penalty that no
> setting inside the container can recover.**
>
> Containers on Docker Desktop run inside a VM behind an internal gateway (`192.168.65.1`, MTU 1500).
> Even `--network host` lands there. SPEC.md §3.3 decision 3 specifies **`network_mode: host` "to
> eliminate NAT overhead"** — that remedy is exactly what Docker Desktop cannot provide, on Windows
> or macOS.
>
> **Deploy one of these instead, in order of preference:**
>
> 1. **Docker on native Linux with `network_mode: host`** — the deployment SPEC.md §3.3 describes.
>    Containers share the host's network stack directly: no VM, no gateway, no penalty. *(The
>    mechanism that costs 5× is absent here; not separately measured on this project.)*
> 2. **Docker inside a WSL2 distribution** (Ubuntu, not Docker Desktop) with
>    `networkingMode=mirrored` — containers then sit on the host's real interface.
> 3. **The binary on the host, outside a container.** 771 MB/s was measured this way.
>
> Docker Desktop remains the right tool for *developing* this project — the test harness, the
> integration suite and the image build all rely on it. The warning is about deploying a
> throughput-sensitive sync onto it.

## The headline

**A container on Docker Desktop for Windows does not get the host's network speed, and no setting
inside the container changes that.** The host reaches **771 MB/s** to the same destination over the
same 10GbE link; a container tops out around **145 MB/s**.

| Path | Throughput |
|---|---|
| **Host → NAS, no container involved** | **771 MB/s** |
| Container → NAS, write (NAT networking) | 125-128 MB/s |
| Container → NAS, write (mirrored networking) | 145-149 MB/s |
| Container ← NAS, read | 43 → 49 MB/s |
| Container ← an SMB share on the *same* Windows machine, read | 42 → 38 MB/s |
| Container ← host folder via bind mount (9p/virtiofs) | 290-312 MB/s |
| Container ← its own filesystem (inside the VM) | 1.7 GB/s |

Note the third row against the fifth: **reading a local folder over SMB is slower than reading it
through a bind mount**, by a factor of six. Both are slow, but the network path is slower.

## What is actually the ceiling

Parallel 1 GiB writes to the destination, before and after switching WSL2 to mirrored networking:

```
                NAT          mirrored
streams=1   128 MiB/s      146 MiB/s
streams=2   128 MiB/s      136 MiB/s
streams=4   128 MiB/s      146 MiB/s
streams=7   128 MiB/s      146 MiB/s      <- 7 GiB, and still the same aggregate
```

**Aggregate throughput does not rise with the number of streams.** Seven concurrent writers move no
more data per second than one. SMB multichannel was swept at `max_channels` 2, 4 and 8 across all of
it and changed nothing meaningful either. Whatever binds is below SMB and shared by every stream.

It is **not** the physical link and **not** routing. The host has a 1 Gbps I226-LM and a 10 Gbps Intel
X710, and the route to the NAS correctly uses the 10 Gbps adapter — which is how the host gets
771 MB/s.

It is **not** WSL2's NAT layer either, which was the first hypothesis and was wrong. Switching to
mirrored networking gained about **14%**, not the several-fold jump expected. The WSL2 VM genuinely
does get the host's interfaces — `eth1: 10.10.20.10/24 mtu 8986`, jumbo frames and all — and the
container still could not use them.

**It is Docker Desktop's own network layer.** Run a container with `--network host`, which should put
it on the VM's stack, and it still reports:

```
eth0: 192.168.65.3/24  mtu 1500
10.10.20.42 via 192.168.65.1 dev eth0
```

`192.168.65.x` is Docker Desktop's internal VM network. Every container packet goes through that
gateway on a 1500-byte MTU, regardless of host networking, mirrored mode, or the 8986-byte jumbo
interface sitting one layer below. That gateway is shared, which is why adding streams adds nothing.

### What would actually lift it

- **Run the engine where the network is native.** Docker installed *inside* a WSL2 distribution
  (Ubuntu, say) rather than Docker Desktop puts containers on the distribution's own stack, which
  under mirrored networking is the host's 10GbE interface.
- **Or run the sync on the host**, outside a container entirely — 771 MB/s was measured that way.
- **Or accept ~145 MB/s** and tune everything else around it. For many jobs this is fine; for a
  multi-hundred-gigabyte fan-out it is the difference between minutes and hours.

Until one of those changes, `workers`, `parallel_destinations` and `multichannel` cannot raise
throughput on this host — the measurements above are as close to a controlled proof as this gets.

## WSL2 mirrored networking

Worth ~14% on its own and it is the prerequisite for the "Docker inside a WSL2 distro" option
above. It is **not** the fix by itself.

**It was applied and then reverted on the machine these numbers came from**, for compatibility and
ease of deployment: 14% did not justify changing the networking semantics of a host running other
services. Nothing broke while it was on — cascade stayed healthy, `host.docker.internal` stayed
reachable, the suite ports all answered — so the revert was a preference, not a repair.

### Apply it

Create `%USERPROFILE%\.wslconfig` (a **user** file, not a repository one):

```ini
[wsl2]
networkingMode=mirrored
```

Then restart the WSL2 VM. This stops every WSL distribution **and every Docker container**:

```powershell
wsl --shutdown
```

Docker Desktop restarts its backend on the next command, but restarting the app is more reliable:

```powershell
Stop-Process -Name "Docker Desktop","com.docker.backend" -Force
Start-Process "C:\Program Files\Docker\Docker\Docker Desktop.exe"
```

Containers whose restart policy is `unless-stopped` or `always` come back by themselves. Anything on
the default `no` policy does **not** — start those again with `docker start <name>`, or bring the
stack up with its compose file. On the machine this was measured on, 4 of 11 containers returned by
themselves and 7 needed starting.

### Verify it worked

```powershell
wsl -d docker-desktop -e sh -c "ip -4 addr show"
```

Under mirrored networking the VM holds the **host's** addresses — here `10.10.20.10/24` with MTU
8986 — instead of a private NAT address. Note again that *containers* will still show
`192.168.65.x`; that is the layer this does not fix.

### Remove it

```powershell
Remove-Item "$env:USERPROFILE\.wslconfig"
wsl --shutdown
```

Or keep the file and be explicit, which is clearer for anyone reading it later:

```ini
[wsl2]
networkingMode=nat
```

Either way `wsl --shutdown` is what applies the change, and it stops containers again.

### What to re-test after switching

Mirrored mode changes more than throughput. After switching, this deployment was checked and all of
it still worked, but check yours:

- **`localhost` and `host.docker.internal`.** The VM shares the host's interfaces, so services that
  relied on the NAT mapping can resolve differently. This project's cn4m callback uses
  `host.docker.internal:2640` — verified reachable from inside the container afterwards.
- **Published ports**, which are reachable on the host's own addresses rather than via a NAT forward.
- **Firewall rules**, which now apply to the VM's traffic as if it were the host's.

## Native Windows: measured

The native build exists because of everything above. Measured on the same host, same 10GbE link, same
1 GiB file:

| | Throughput |
|---|---|
| Containerised (Docker Desktop) | 145 MB/s |
| **Native `.exe`** | **~434 MB/s** |

1 GiB copied through the whole application — scan, diff, temp file, fsync, rename, mtime — in 2.36
seconds, against a real NAS over SMB 3.1.1. Mounting and listing the share took 23 ms.

That is **3x**, and it is not the ceiling: the source in that test was a local disk path, so the
figure is bounded by disk and by a single stream, not by the network layer that capped the container.

### How the native build differs

- **Nothing is mounted.** Windows opens `\\host\share\path` directly, so the connector authenticates
  the session with `WNetAddConnection2` and hands that UNC path to the engine. There is no
  `MOUNT_ROOT`, no mountpoint, and no startup mount cleanup.
- **Credentials never reach disk.** `WNetAddConnection2` takes them in memory. The 0600 temp file
  `mount.cifs` requires does not exist on this path — better than the container, not a compromise.
- **Local target paths are ordinary Windows paths** (`M:\projects\repo`), not `/mnt/local`. A job
  definition is therefore *not* portable between a container and a native install, because the local
  paths differ; SMB targets are.
- **A destination another process holds open is skipped**, named in the log, summarised at
  completion, and not retried (SPEC.md §6.2). This is a Windows-only condition — Linux replaces open
  files without complaint.

### Running it

```powershell
$env:ENCRYPTION_KEY = "a long random key"     # required
$env:DATA_DIR = "C:\ProgramData\cn4m-cascade"  # the default; holds the database
.\cn4m-cascade.exe
```

Then open <http://localhost:2649>. `DATA_DIR` defaults to `%ProgramData%\cn4m-cascade`, which is
deliberate: a service account and an interactive operator must see the same database.

Back up `DATA_DIR` and `ENCRYPTION_KEY` together — the same rule as the container, for the same
reason.

### Running it as a service

The foreground process is the default and needs nothing. To have it start at boot and survive logout:

```powershell
# From an ELEVATED prompt:
.\cn4m-cascade.exe -service install
.\cn4m-cascade.exe -service start
```

The key can come from either of the two places a service can actually read: the **`.env` beside the
executable** (the same file the foreground process uses), or the **machine** environment via
`setx /M ENCRYPTION_KEY "<value>"`. A service does **not** inherit the environment of the prompt that
installed it, so setting it for one shell is not enough.

`-service install` checks for one of those two **before touching the service manager** and names both
if neither is present — a service that installs cleanly and then refuses to start is the failure this
avoids. The subcommand name and that check are both validated without administrator rights, so a typo
does not come back as "Access is denied".

To stop or remove it:

```powershell
.\cn4m-cascade.exe -service stop
.\cn4m-cascade.exe -service uninstall
```

`-service stop` waits until the service has actually stopped rather than returning as soon as the
request is sent. `uninstall` removes the service only: the database and your `ENCRYPTION_KEY` are
left alone.

Details worth knowing:

- **Start type is automatic**, so it comes back after a reboot.
- **Stopping is given a 45-second hint.** SPEC.md §10 budgets 30 seconds for graceful shutdown
  because lazily detaching a share whose server has gone is the slow case; a shorter hint would have
  Windows kill the process in exactly the situation that budget exists for.
- **Start, stop and unexpected exits are written to the Event Log** as well as to slog's JSON, because
  a service with no console has nowhere else to say why it stopped.
- **An unexpected exit returns a non-zero code**, which is what makes the service's recovery settings
  ("restart on failure") apply.
- **Which mode it runs in is detected, not configured** (`svc.IsWindowsService`). There is no flag to
  forget.

Build it reproducibly with `make windows-exe`, which cross-compiles into `./dist` from the dev
container — CGO is off and SQLite is pure Go, so no Windows host is needed to produce the binary.

## Other findings, unrelated to the network

**A host bind mount is not fast storage.** Reading from a Docker Desktop bind mount measured 220–312
MB/s against 1.7 GB/s for the container's own filesystem. That is the 9p/virtiofs file-sharing layer,
not the disk — the same physical disk backs both. For a job whose source is a local folder, this is
the ceiling, and it is a *different* ceiling from the network one above.

**Reading the source over SMB instead is worse, not better.** It was predicted to help, on the
reasoning that it skips the 9p layer. Measured, it was seven times slower — 42 MB/s versus ~300 MB/s
— because SMB crosses the NAT boundary that a bind mount does not. Do not switch a local source to
SMB to chase speed.

**What the job settings actually do**, for one large file going to several destinations:

| Setting | Effect on a single large file |
|---|---|
| `workers` | **None.** The executor queues one action per *file*; one file is one action, so a single worker copies it and the rest idle. It is a many-files lever. |
| `parallel_destinations` | The real lever — turns N sequential copies into N concurrent ones. Its ceiling is then the shared source read, or the link. |
| `multichannel` | Raises one destination's throughput above a single TCP stream. Specifying it applies `max_channels=2` by default on this kernel; more needs `max_channels=N` explicitly, plus server support. |

**The source is read once per destination.** Copying one file to seven destinations reads the source
seven times, with only the page cache deduplicating them. A read-once-write-many pipeline would make
that a fixed cost; it does not exist today.

## Re-running the benchmark

Needs a credentials file — **never credentials on the command line**, which is a hard rule of this
project. Two lines, no markdown fences, no quotes:

```ini
username=YOURUSER
password=YOURPASS
```

Then, adjusting the two shares:

```bash
docker run --rm --privileged \
  -v "/path/to/creds:/tmp/c:ro" \
  --entrypoint sh cn4m-cascade:latest -c '
    mkdir -p /mnt/d
    mount.cifs //NAS/share /mnt/d -o credentials=/tmp/c,vers=3.1.1,rsize=4194304,wsize=4194304
    mkdir -p /mnt/d/test-dest
    for N in 1 2 4 7; do
      start=$(date +%s); i=1
      while [ $i -le $N ]; do
        dd if=/dev/zero of=/mnt/d/test-dest/p$i.bin bs=4M count=256 2>/dev/null &
        i=$((i+1))
      done
      wait; sync; end=$(date +%s)
      secs=$((end-start)); [ $secs -lt 1 ] && secs=1
      echo "streams=$N  $((N*1024))MiB in ${secs}s  =>  $((N*1024/secs)) MiB/s"
    done
    rm -f /mnt/d/test-dest/p*.bin; umount -l /mnt/d'
```

`--privileged` is for `mount.cifs` and for dropping the page cache between read tests; the shipping
image needs only `SYS_ADMIN` and `DAC_READ_SEARCH` to mount. Delete the credentials file afterwards.

**Read the aggregate, not the per-stream number.** A flat aggregate as the stream count rises means a
saturated link; an aggregate that climbs means you have not found the ceiling yet.
