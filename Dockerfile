# Production image (SPEC.md §10).
#
# Three stages: Node builds the SPA, Go embeds it and links a static binary,
# and a slim Debian carries the result plus the one thing this program cannot
# work without — mount.cifs.
#
# No fixed GOARCH anywhere. Each host builds for itself, so Windows and macOS
# both produce a native image with no cross-compilation and no buildx setup.

# ---------- 1. The SPA ----------
FROM node:22-bookworm AS web

WORKDIR /web
# Manifests first, so a source-only change does not re-run the install.
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY web/ ./
RUN npm run build


# ---------- 2. The binary ----------
FROM golang:1.25-bookworm AS build

WORKDIR /src
# Same trick: dependencies resolve from the module files alone.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The SPA is embedded via web/embed.go, so it has to exist before the compile.
COPY --from=web /web/dist ./web/dist

# CGO off gives a static binary that runs on a slim image with no glibc
# gymnastics; modernc.org/sqlite is pure Go, so nothing is lost by it.
# Trimpath and the linker flags drop paths and debug tables that only make the
# image bigger.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/cn4m-cascade ./cmd/cn4m-cascade


# ---------- 3. The image ----------
#
# Alpine, not debian:bookworm-slim, and the reason is arithmetic: the Debian
# slim base is 97 MB on arm64 before a single package is installed, so §10's
# "well under 100 MB" is unreachable with it rather than merely missed.
#
# The parity argument for matching the bookworm dev container turned out to be
# weaker than it looked. The binary is static Go with CGO off, so musl versus
# glibc does not reach it at all; the CIFS behaviour this project cares about —
# the dialect ladder, multichannel fallback, the uninterruptible-syscall hangs
# of §5 — is kernel behaviour, and the kernel is the host's either way.
# mount.cifs is the same upstream cifs-utils on both.
#
# That is an argument, not evidence, which is why the 6a exit criteria require
# this image to mount a real CIFS share rather than merely to start.
FROM alpine:3.20

# cifs-utils provides mount.cifs, which is the whole point of the program.
# ca-certificates is not hygiene either: outbound callbacks (SPEC.md §8.2) POST
# to arbitrary https:// URLs, and every one fails with a certificate error
# without a CA bundle.
#
# No tzdata package: the zone database is compiled into the binary
# (`time/tzdata`), so TZ resolves identically on any base image and cannot be
# broken by a slimmed-down one.
RUN apk add --no-cache cifs-utils ca-certificates

COPY --from=build /out/cn4m-cascade /usr/local/bin/cn4m-cascade

# Runs as root, which is not a shortcut: mount.cifs requires CAP_SYS_ADMIN, and
# the container is granted it (§10). Files written into *local* targets are
# root-owned as a result; MOUNT_UID/MOUNT_GID cover the SMB side. See the
# README.
WORKDIR /data
VOLUME ["/data"]
EXPOSE 2649

# The binary answers its own healthcheck: this image has no curl or wget, and
# adding a network tool purely so Docker can ask "are you alive?" is not worth
# the surface.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/cn4m-cascade", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/cn4m-cascade"]
