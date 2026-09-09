# Every Go command runs inside the dev container: the host may be macOS, where
# there is no Go toolchain and no mount.cifs (PROGRESS.md B-1/B-2).

COMPOSE := docker compose -f docker-compose.test.yml

# The production stack, deliberately a different compose project from the
# harness above. `make run` used to exec into the *test* container, which is
# how running the server and running the suite came to collide (PROGRESS.md
# §7a, §7d). They now share nothing.
PROD := docker compose -f docker-compose.yml

# How to reach the dev container. Overridable because the compose one can end
# up wedged and unkillable — CIFS threads in uninterruptible D state survive
# `docker rm -f`, and only a Docker daemon restart clears them (PROGRESS.md
# §7a, §7c). The documented workaround is an ad-hoc container from the same
# image, which everything here then works against:
#
#   make test-integration DEV="docker exec -i cn4m-cascade-dev-tmp"
DEV ?= $(COMPOSE) exec -T dev

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-18s %s\n", $$1, $$2}'

# The host folder exposed to the container as /mnt/local, so a "local" target
# can point at real files on your machine. Override in your shell or a .env
# file; see docker-compose.test.yml for the full explanation.
CN4M_CASCADE_LOCAL_DIR ?= $(if $(HOME),$(HOME),$(USERPROFILE))/cn4m
export CN4M_CASCADE_LOCAL_DIR

.PHONY: local-dir
local-dir: ## Create the host folder shared with the container as /mnt/local
	@mkdir -p "$(CN4M_CASCADE_LOCAL_DIR)" && echo "local folder: $(CN4M_CASCADE_LOCAL_DIR) -> /mnt/local"

.PHONY: harness-up
harness-up: local-dir ## Build and start the Samba servers + dev container
	$(COMPOSE) up -d --build
	$(DEV) bash /src/test/wait-for-samba.sh

.PHONY: harness-down
harness-down: ## Stop the harness and remove its volumes
	$(COMPOSE) down -v

.PHONY: dev-shell
dev-shell: ## Interactive shell in the dev container
	$(COMPOSE) exec dev bash

.PHONY: verify-cifs
verify-cifs: ## PROGRESS.md B-4: prove the host kernel can do CIFS mounts
	$(DEV) sh -c 'mkdir -p /mnt/smb/_verify \
	  && mount -t cifs //172.28.0.10/public /mnt/smb/_verify -o guest,vers=3.1.1 \
	  && ls -la /mnt/smb/_verify \
	  && umount /mnt/smb/_verify \
	  && echo "CIFS OK"'

.PHONY: verify-image
verify-image: image ## Prove the PRODUCTION image can mount CIFS (SPEC.md §11, 6a)
	@# Needs the Samba harness: it mounts a real share on the harness network.
	@docker network inspect cn4m-cascade_smbnet >/dev/null 2>&1 || { \
	  echo "The test harness is not up — this mounts a real share from it."; \
	  echo "Run: make harness-up"; exit 1; }
	@# Distinct from verify-cifs, which proves the *kernel* can mount using the
	@# Debian dev container. This proves the alpine image that actually ships
	@# can do it: same kernel, different userspace mount.cifs and a different
	@# libc. Both questions are real and only one of them is about the host.
	@docker run --rm \
	  --cap-add SYS_ADMIN --cap-add DAC_READ_SEARCH --security-opt apparmor:unconfined \
	  --network cn4m-cascade_smbnet --entrypoint sh cn4m-cascade:latest -c '\
	    mkdir -p /mnt/probe \
	    && mount.cifs //172.28.0.10/public /mnt/probe -o guest,vers=3.1.1 \
	    && ls /mnt/probe >/dev/null \
	    && echo probe > /mnt/probe/.image-probe && rm -f /mnt/probe/.image-probe \
	    && umount /mnt/probe \
	    && echo "PRODUCTION IMAGE CIFS OK — mounted, listed, wrote, unmounted"'

.PHONY: tidy
tidy: ## Resolve dependencies (writes go.sum)
	$(DEV) go mod tidy

# The SPA (SPEC.md §9). Node lives in the dev image as a build-time dependency
# only: the binary embeds the compiled assets and the production image ships no
# JavaScript toolchain.
.PHONY: web-install
web-install: ## Install frontend dependencies (npm ci)
	$(DEV) sh -c 'cd /src/web && npm ci --no-audit --no-fund'

.PHONY: web-build
web-build: ## Build the SPA into web/dist, which web/embed.go embeds
	$(DEV) sh -c 'cd /src/web && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && npm run build'

.PHONY: web-lint
web-lint: ## Type-check the SPA
	$(DEV) sh -c 'cd /src/web && npm run lint'

# Unlike every other target these run in the foreground and want a terminal,
# so they use `exec` rather than `exec -T`: without a TTY, ^C never reaches the
# process and the only way to stop it is to kill the container.
.PHONY: web-dev
web-dev: ## Vite dev server on http://localhost:5173 (proxies /api to the Go server)
	$(COMPOSE) exec dev sh -c 'cd /src/web && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && npm run dev -- --host 0.0.0.0'

.PHONY: run
run: require-env image ## Run the production image on http://localhost:2649
	$(PROD) up -d
	@echo "cn4m cascade is on http://localhost:2649  —  make logs / make down"

.PHONY: require-env
require-env:
	@# A prerequisite of its own, so a missing .env is reported in a second
	@# rather than after a full image build.
	@test -f .env || { \
	  echo "No .env file. Create one with at least:"; \
	  echo "  ENCRYPTION_KEY=$$(head -c 24 /dev/urandom | base64 | tr -d '=+/' | head -c 32)"; \
	  echo "and optionally CN4M_CASCADE_ADMIN_PASSWORD, TZ, CN4M_CASCADE_STATUS_URL."; \
	  exit 1; }

.PHONY: image
image: ## Build the production image (SPEC.md §10)
	docker build -t cn4m-cascade:latest .
	@docker images cn4m-cascade:latest --format '  image size: {{.Size}}'

.PHONY: down
down: ## Stop the production stack
	$(PROD) down

.PHONY: logs
logs: ## Follow the production server's logs
	$(PROD) logs -f

.PHONY: dev-run
dev-run: ## Run the server from source in the dev container, on :12649 (collides with test-integration)
	@echo 'Serving from source at http://localhost:12649 — production is: make run (on :2649)'
	$(COMPOSE) exec dev sh -c 'cd /src && go run ./cmd/cn4m-cascade'

.PHONY: lint
lint: ## golangci-lint
	$(DEV) golangci-lint run

.PHONY: test-unit
test-unit: ## Unit tests under the race detector (no kernel, no Samba)
	$(DEV) env CGO_ENABLED=1 go test -race -count=1 ./...

# A test process killed before its cleanup ran (a timeout, ^C, a cancelled CI
# job) leaves two things behind that poison every later run: an iptables
# blackhole from the cable-pull test, and CIFS mounts that retry forever. The
# retries are the nastier of the two — while the kernel is mid-reconnect to a
# server, a *new* mount to it returns error 115, which reads exactly like a
# broken change rather than a dirty harness.
.PHONY: harness-clean
# Removing the blackhole signal files is enough to disarm an orphaned helper:
# each watches one specific path, so with its files gone it can never fire and
# simply idles out inside its bounded life. Deliberately no `pkill` — an
# earlier version ran `pkill -f "blackhole-.*"`, which matched the very shell
# running it (the pattern appears in its own command line) and killed
# harness-clean mid-run with exit 143.
harness-clean: ## Clear leftover mounts and blackholes from a killed test run
	@$(DEV) sh -c 'mount -t cifs 2>/dev/null | awk "{print \$$3}" | grep -E "^/tmp/(Test|cn4m-mnt-)" \
	  | while read m; do umount -l "$$m" 2>/dev/null && echo "unmounted $$m"; done; \
	  rmdir /tmp/cn4m-mnt-*/* /tmp/cn4m-mnt-* 2>/dev/null; \
	  rm -f /tmp/blackhole-* 2>/dev/null; \
	  for ip in 172.28.0.10 172.28.0.11; do \
	    while iptables -D OUTPUT -d $$ip -j DROP 2>/dev/null; do echo "removed blackhole on $$ip"; done; \
	  done; true'

# -timeout is explicit because the default one has been observed not to fire:
# a run wedged in TestDestinationDisappearsMidRun with a leftover blackhole
# parked below the point where Go's watchdog can act, and simply sat there
# rather than dumping stacks. The point is a stack dump instead of an
# indefinite park.
#
# Raised from 15m to 25m in Phase 6a. The original figure was "comfortably
# above the ~5.5m the suite takes", and that stopped being true as the suite
# grew from 53 tests to 85 — a run is now ~10m, and TestDestinationDisappearsMidRun
# alone can spend several minutes in cleanup when its destination is wedged.
# A 15m budget started killing the suite outright, which loses every result
# rather than one test's.
.PHONY: test-integration
test-integration: harness-clean web-build ## Integration tests against the Samba harness
	@# A container's environment is fixed at creation, so a variable renamed in
	@# the compose file does not reach a container that is already running. The
	@# suite skips every Samba test when it cannot find these, and reports
	@# `ok` with exit 0 for having done nothing — which is how a rename once
	@# produced a green run of 9 PASS and 70 SKIP (PROGRESS.md §7c). Refuse
	@# outright rather than pass vacuously.
	@$(DEV) sh -c 'test -n "$$CN4M_CASCADE_TEST_SAMBA_A" && test -n "$$CN4M_CASCADE_TEST_SAMBA_B"' \
	  || { echo "CN4M_CASCADE_TEST_SAMBA_A/B are not set in the dev container."; \
	       echo "The container predates the current docker-compose.test.yml."; \
	       echo "Recreate it:  docker compose -f docker-compose.test.yml up -d --force-recreate dev"; \
	       exit 1; }
	$(DEV) env CGO_ENABLED=1 go test -race -tags=integration -count=1 -timeout 25m -v ./test/...

.PHONY: test-scale
test-scale: ## The 100k-file mirror exit criterion (slow: several minutes)
	$(DEV) env CN4M_CASCADE_SCALE_FILES=100000 go test -tags=integration -count=1 -v -timeout 60m \
	  -run TestScaleMirror ./test/...

.PHONY: test
test: test-unit test-integration ## All tests (excludes test-scale)

.PHONY: build
build: web-build ## Compile the server binary with the SPA embedded
	$(DEV) go build -o /tmp/cn4m-cascade ./cmd/cn4m-cascade

.PHONY: demo
demo: build ## Walk the Phase 1 exit criteria with curl and print every response
	$(DEV) bash /src/test/exit-criteria.sh
