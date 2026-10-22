GO_BIN ?= go
REWRITE_CACHE ?= $(CURDIR)/.cache/rewrite
REWRITE_VERSION ?= dev
REWRITE_ARCHITECTURES ?= amd64,arm64
REWRITE_OUTPUT ?= $(CURDIR)/dist/rewrite
REWRITE_BUILD_FLAGS ?=
export GO_BIN
export GOCACHE := $(REWRITE_CACHE)/go-build
export GOMODCACHE := $(REWRITE_CACHE)/go-mod
export POWERLINUX_BUILD_CACHE := $(REWRITE_CACHE)

.PHONY: rewrite-deps rewrite-ui rewrite-test rewrite-install-test rewrite-mysql-test rewrite-build rewrite-dev
rewrite-deps:
	mkdir -p "$(GOCACHE)" "$(GOMODCACHE)" "$(REWRITE_CACHE)/npm"
	$(GO_BIN) mod download
	npm ci --prefix frontend --cache "$(REWRITE_CACHE)/npm" --no-audit --no-fund

rewrite-ui:
	npm run typecheck --prefix frontend
	NEXT_TELEMETRY_DISABLED=1 npm run build --prefix frontend

rewrite-install-test:
	python3 scripts/rewrite/sync-installer-ui.py --check
	python3 -m unittest discover -s scripts/rewrite/tests -p 'test_*.py' -v

rewrite-mysql-test:
	$(GO_BIN) -C third_party/mysql test -mod=readonly -race -count=1 ./...

rewrite-test: rewrite-install-test rewrite-mysql-test
	$(GO_BIN) test -race -count=1 ./...
	npm test --prefix frontend
	npm run typecheck --prefix frontend

rewrite-build:
	bash scripts/rewrite/build.sh --version "$(REWRITE_VERSION)" --architectures "$(REWRITE_ARCHITECTURES)" --output "$(REWRITE_OUTPUT)" $(REWRITE_BUILD_FLAGS)

rewrite-dev:
	bash scripts/rewrite/dev.sh
