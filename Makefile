.PHONY: build install test clean run dev fmt vet tidy dist \
		verify-version verify-dist \
		bump-patch bump-minor bump-major sync-version print-version \
		release-patch release-minor release-major

BIN_NAME := metis
PKG := github.com/Ricardo-M-L/metis

# ---- Version source of truth -------------------------------------------------
# A single VERSION file holds the current number. `make build` / `make install`
# do NOT touch it — they just read. To advance the version number, run one of
# the explicit bump targets below.
#
VERSION_FILE := VERSION
VERSION := $(shell cat $(VERSION_FILE) 2>/dev/null || echo "0.1.0")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
MAKEFILE_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
VERIFY_DIST := $(MAKEFILE_DIR)/scripts/verify-dist.sh
LOCAL_INSTALL_DIR ?= $(HOME)/.local/bin
LOCAL_INSTALL_PATH := $(LOCAL_INSTALL_DIR)/$(BIN_NAME)
LOCAL_INSTALL_SCRIPT := $(MAKEFILE_DIR)/scripts/install-local-build.sh

LDFLAGS := -ldflags "-s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)"

build:
	go build $(LDFLAGS) -o bin/$(BIN_NAME) ./cmd/metis

install: build
	@"$(LOCAL_INSTALL_SCRIPT)" "$(CURDIR)/bin/$(BIN_NAME)" "$(LOCAL_INSTALL_PATH)"

run: build
	./bin/$(BIN_NAME)

dev:
	go run ./cmd/metis

test:
	go test -race -coverprofile=coverage.txt ./...

fmt:
	gofmt -s -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin dist

print-version:
	@echo $(VERSION)

verify-version:
	@"$(VERIFY_DIST)" --repo "$(CURDIR)" --metadata-only $(if $(TAG),--tag "$(TAG)")

# ---- Version bumping (opt-in) -----------------------------------------------
# Default behavior: build/install never advance the version. To bump, run one
# of these explicitly. Each target rewrites VERSION, the Go source default,
# npm package metadata, and both native Desktop version sources so all five
# release version declarations stay in sync.
#
#   make bump-patch        0.1.0 -> 0.1.1
#   make bump-minor        0.1.5 -> 0.2.0
#   make bump-major        0.2.3 -> 1.0.0
#   make release-patch     bump-patch + build
#
# During testing, prefer `make build`. `make install` leaves the version alone,
# installs one local binary only, and refuses to replace a curl-managed release.

bump-patch:
	@cur=$$(cat $(VERSION_FILE)); \
	new=$$(echo $$cur | awk -F. '{printf "%d.%d.%d", $$1, $$2, $$3+1}'); \
	$(MAKE) --no-print-directory _do-bump CUR=$$cur NEW=$$new

bump-minor:
	@cur=$$(cat $(VERSION_FILE)); \
	new=$$(echo $$cur | awk -F. '{printf "%d.%d.0", $$1, $$2+1}'); \
	$(MAKE) --no-print-directory _do-bump CUR=$$cur NEW=$$new

bump-major:
	@cur=$$(cat $(VERSION_FILE)); \
	new=$$(echo $$cur | awk -F. '{printf "%d.0.0", $$1+1}'); \
	$(MAKE) --no-print-directory _do-bump CUR=$$cur NEW=$$new

# Internal target — call via bump-* so $(NEW) is set. Keep the CLI, npm
# installer, and native Desktop metadata on the same release version.
_do-bump:
	@if [ -z "$(NEW)" ]; then echo "usage: make bump-{patch,minor,major}" >&2; exit 1; fi
	@echo "$(NEW)" > $(VERSION_FILE)
	@sed -i.bak -E 's/Version = "[^"]*"/Version = "$(NEW)"/' internal/version/version.go && rm internal/version/version.go.bak
	@sed -i.bak -E 's/"version": "[^"]*"/"version": "$(NEW)"/' install/npm/package.json && rm install/npm/package.json.bak
	@sed -i.bak -E 's/"productVersion": "[^"]*"/"productVersion": "$(NEW)"/' metis-desktop/wails.json && rm metis-desktop/wails.json.bak
	@sed -i.bak -E 's/return "[0-9]+\.[0-9]+\.[0-9]+"/return "$(NEW)"/' metis-desktop/app.go && rm metis-desktop/app.go.bak
	@echo "version: $(CUR) -> $(NEW)"

release-patch: bump-patch
	@$(MAKE) --no-print-directory build

release-minor: bump-minor
	@$(MAKE) --no-print-directory build

release-major: bump-major
	@$(MAKE) --no-print-directory build

# Cross-compile the GitHub Release assets. Unix targets use tar.gz; Windows
# targets use zip so the archive works with built-in PowerShell tooling. Raw
# binaries are removed after packing so dist/ is exactly the 12 files uploaded
# to GitHub (six archives and six checksum sidecars).
dist: clean verify-version
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-darwin-arm64  ./cmd/metis
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-darwin-amd64  ./cmd/metis
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-linux-arm64   ./cmd/metis
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-linux-amd64   ./cmd/metis
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-windows-arm64.exe ./cmd/metis
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-windows-amd64.exe ./cmd/metis
	cd dist || exit 1; set -e; for f in $(BIN_NAME)-darwin-* $(BIN_NAME)-linux-*; do \
		tar czf $$f.tar.gz $$f && shasum -a 256 $$f.tar.gz > $$f.tar.gz.sha256 && rm $$f; \
	done
	cd dist || exit 1; set -e; for f in $(BIN_NAME)-windows-*.exe; do \
		archive=$${f%.exe}.zip; cp $$f metis.exe; zip -q $$archive metis.exe; rm metis.exe; \
		shasum -a 256 $$archive > $$archive.sha256; rm $$f; \
	done

verify-dist: dist
	@"$(VERIFY_DIST)" --repo "$(CURDIR)" $(if $(TAG),--tag "$(TAG)")
