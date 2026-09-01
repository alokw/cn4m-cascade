# Every Go command runs inside the dev container: the host may be macOS, where
# there is no Go toolchain and no mount.cifs (PROGRESS.md B-1/B-2).

COMPOSE := docker compose -f docker-compose.test.yml
DEV     := $(COMPOSE) exec -T dev

.PHONY: help
help:
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-18s %s\n", $$1, $$2}'

.PHONY: harness-up
harness-up: ## Build and start the Samba servers + dev container
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

.PHONY: tidy
tidy: ## Resolve dependencies (writes go.sum)
	$(DEV) go mod tidy

.PHONY: lint
lint: ## golangci-lint
	$(DEV) golangci-lint run

.PHONY: test-unit
test-unit: ## Unit tests under the race detector (no kernel, no Samba)
	$(DEV) env CGO_ENABLED=1 go test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Integration tests against the Samba harness
	$(DEV) env CGO_ENABLED=1 go test -race -tags=integration -count=1 -v ./test/...

.PHONY: test-scale
test-scale: ## The 100k-file mirror exit criterion (slow: several minutes)
	$(DEV) env SMBSYNC_SCALE_FILES=100000 go test -tags=integration -count=1 -v -timeout 60m \
	  -run TestScaleMirror ./test/...

.PHONY: test
test: test-unit test-integration ## All tests (excludes test-scale)

.PHONY: build
build: ## Compile the server binary
	$(DEV) go build -o /tmp/smbsync ./cmd/smbsync

.PHONY: demo
demo: build ## Walk the Phase 1 exit criteria with curl and print every response
	$(DEV) bash /src/test/exit-criteria.sh
