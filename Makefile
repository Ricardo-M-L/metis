.PHONY: build install test clean run dev fmt vet tidy dist \
        bump-patch bump-minor bump-major sync-version print-version \
        release-patch release-minor release-major

BIN_NAME := metis
PKG := github.com/Ricardo-M-L/metis

# ---- Version source of truth -------------------------------------------------
# A single VERSION file holds the current number. `make build` / `make install`
# do NOT touch it — they just read. To advance the version number, run one of
# the explicit bump targets below.
#
# If we're inside a real git repo with tags, `git describe` overrides the file
# (and gets the dirty-suffix etc for free).
VERSION_FILE := VERSION
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || cat $(VERSION_FILE) 2>/dev/null || echo "0.1.0")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -ldflags "-s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)"

build:
	go build $(LDFLAGS) -o bin/$(BIN_NAME) ./cmd/metis

install: build
	install -m 0755 bin/$(BIN_NAME) $(HOME)/.local/bin/$(BIN_NAME)
	@# Also install via `go install` so $(GOBIN) (typically ~/go/bin) gets
	@# the same build with version metadata injected — without this step,
	@# users whose PATH puts ~/go/bin before ~/.local/bin would run a stale
	@# `go install` build that lacks the build-time Date.
	go install $(LDFLAGS) ./cmd/metis

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

# ---- Version bumping (opt-in) -----------------------------------------------
# Default behavior: build/install never advance the version. To bump, run one
# of these explicitly. Each target rewrites VERSION + the source default in
# internal/version/version.go + npm package.json so all three stay in sync.
#
#   make bump-patch        0.1.0 -> 0.1.1
#   make bump-minor        0.1.5 -> 0.2.0
#   make bump-major        0.2.3 -> 1.0.0
#   make release-patch     bump-patch + install
#
# During testing keep using `make build` / `make install` — they leave the
# version number alone.

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

# Internal target — call via bump-* so $(NEW) is set.
_do-bump:
	@if [ -z "$(NEW)" ]; then echo "usage: make bump-{patch,minor,major}" >&2; exit 1; fi
	@echo "$(NEW)" > $(VERSION_FILE)
	@sed -i.bak -E 's/Version = "[^"]*"/Version = "$(NEW)"/' internal/version/version.go && rm internal/version/version.go.bak
	@sed -i.bak -E 's/"version": "[^"]*"/"version": "$(NEW)"/' install/npm/package.json && rm install/npm/package.json.bak
	@echo "version: $(CUR) -> $(NEW)"

release-patch: bump-patch install
release-minor: bump-minor install
release-major: bump-major install

# Cross-compile the GitHub Release assets. Unix targets use tar.gz; Windows
# targets use zip so the archive works with built-in PowerShell tooling.
dist: clean
	mkdir -p dist
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-darwin-arm64  ./cmd/metis
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-darwin-amd64  ./cmd/metis
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-linux-arm64   ./cmd/metis
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-linux-amd64   ./cmd/metis
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-windows-arm64.exe ./cmd/metis
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BIN_NAME)-windows-amd64.exe ./cmd/metis
	cd dist && for f in $(BIN_NAME)-darwin-* $(BIN_NAME)-linux-*; do \
		tar czf $$f.tar.gz $$f && shasum -a 256 $$f.tar.gz > $$f.tar.gz.sha256; \
	done
	cd dist && for f in $(BIN_NAME)-windows-*.exe; do \
		archive=$${f%.exe}.zip; cp $$f metis.exe; zip -q $$archive metis.exe; rm metis.exe; \
		shasum -a 256 $$archive > $$archive.sha256; \
	done
