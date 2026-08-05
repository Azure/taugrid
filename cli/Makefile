.PHONY: build install test test-kind-e2e lint clean verify-release-inputs release-binaries release-assets

BINARY := tau
GEN_BINARY := tau-gen
PKG := ./cmd/tau
GEN_PKG := ./cmd/tau-gen
VERSION ?= $(shell git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || echo unknown)
VERSION_PKG := github.com/Azure/taugrid/core/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).Date=$(DATE)
BUILD_FLAGS := -trimpath -mod=readonly -buildvcs=false

# Pinned so a staticcheck release cannot fail CI on an unrelated change. Bump it
# deliberately, in a PR that also fixes whatever the new version finds.
STATICCHECK_VERSION := v0.7.0
RELEASE_DIR := bin/release
RELEASE_TARGETS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
DARWIN_MIN_VERSION := 12.0
DARWIN_CGO_CFLAGS := -O2 -g -mmacosx-version-min=$(DARWIN_MIN_VERSION)
DARWIN_CGO_LDFLAGS := -mmacosx-version-min=$(DARWIN_MIN_VERSION)

build:
	go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)
	go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/$(GEN_BINARY) $(GEN_PKG)

install:
	go install $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" $(PKG)
	go install $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" $(GEN_PKG)

test:
	go test ./...

test-kind-e2e: build
	TAU_KIND_SKIP_BUILD=1 ./scripts/kind-smoke-e2e.sh

test-integration: build
	go test -tags=integration -v -timeout=15m ./internal/cli/...

lint:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

clean:
	rm -rf bin/

# Release builds require explicit SemVer and exact source metadata. The tag
# workflow separately proves that the checkout is clean and VERSION names an
# annotated tag on main.
verify-release-inputs:
	@printf '%s\n' "$(VERSION)" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$$' || \
	  { echo "VERSION must be SemVer with a v prefix, got '$(VERSION)'" >&2; exit 1; }
	@printf '%s\n' "$(COMMIT)" | grep -Eq '^[0-9a-f]{40}$$' || \
	  { echo "COMMIT must be a full 40-character git SHA, got '$(COMMIT)'" >&2; exit 1; }
	@test "$(COMMIT)" = "$$(git rev-parse HEAD)" || \
	  { echo "COMMIT $(COMMIT) does not match HEAD $$(git rev-parse HEAD)" >&2; exit 1; }
	@test "$$(go env GOHOSTOS)" = "darwin" || \
	  { echo "release builds require macOS because Darwin token-cache support requires CGO" >&2; exit 1; }

# Cross-compile the public release surface. Outputs raw binaries matching the
# established GitHub Release asset contract.
release-binaries: verify-release-inputs
	@test -n "$(RELEASE_DIR)" || { echo "RELEASE_DIR must not be empty" >&2; exit 1; }
	@rm -rf "$(RELEASE_DIR)"
	@mkdir -p "$(RELEASE_DIR)"
	@for target in $(RELEASE_TARGETS); do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  if [ "$$os" = "darwin" ]; then \
	    cgo=1; cgo_cflags="$(DARWIN_CGO_CFLAGS)"; cgo_ldflags="$(DARWIN_CGO_LDFLAGS)"; \
	  else \
	    cgo=0; cgo_cflags=""; cgo_ldflags=""; \
	  fi; \
	  for spec in "$(BINARY):$(PKG)" "$(GEN_BINARY):$(GEN_PKG)"; do \
	    name=$${spec%%:*}; pkg=$${spec#*:}; \
	    out="$(RELEASE_DIR)/$$name-$$os-$$arch"; \
	    echo "build $$out"; \
	    MACOSX_DEPLOYMENT_TARGET=$(DARWIN_MIN_VERSION) CGO_CFLAGS="$$cgo_cflags" CGO_LDFLAGS="$$cgo_ldflags" \
	      GOOS=$$os GOARCH=$$arch CGO_ENABLED=$$cgo \
	      go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o "$$out" "$$pkg" || exit 1; \
	  done; \
	done
	@ls -lh "$(RELEASE_DIR)"/

release-assets: release-binaries
	@(cd "$(RELEASE_DIR)" && shasum -a 256 tau-* > SHA256SUMS)
	@ls -lh "$(RELEASE_DIR)"/tau-* "$(RELEASE_DIR)"/SHA256SUMS
