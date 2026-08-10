# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

.PHONY: help build test lint check license-headers \
	install-tau install-tau-cli install-tau-sdk \
	install-taugrid-portal uninstall-tau uninstall-taugrid-portal \
	tau-docs-build tau-docs-check tau-docs-serve

REPO_ROOT := $(abspath $(dir $(firstword $(MAKEFILE_LIST))))
TAU_GO_DIR := $(REPO_ROOT)/cli
CORE_DIR := $(REPO_ROOT)/core
TAUGRID_PORTAL_DIR := $(REPO_ROOT)/portal
TAU_CORE_CONTROLLER_DIR := $(REPO_ROOT)/controllers/tau-core
GPU_HEALTH_CHECKER_DIR := $(REPO_ROOT)/monitoring/gpu-health-checker
GPU_METRICS_COLLECTOR_DIR := $(REPO_ROOT)/monitoring/gpu-metrics-collector
TAU_SDK_DIR := $(REPO_ROOT)/sdk/python/python
TAU_SITE_DIR := $(REPO_ROOT)/site
PYTHON ?= python3
HOST_GOARCH := $(shell go env GOARCH)
STATICCHECK_VERSION := v0.7.0
GO_COMMAND_FLAGS := -mod=readonly -buildvcs=false

help:
	@echo "TauGrid repository targets:"
	@echo ""
	@echo "  make build                   # build first-party Go components"
	@echo "  make test                    # run Go and Python unit tests"
	@echo "  make lint                    # lint Go and Python source"
	@echo "  make check                   # run build, test, lint, and license checks"
	@echo "  make install-tau             # install the Tau CLI and optional Python SDK"
	@echo "  make install-tau-cli         # install the Tau CLI and tau-gen"
	@echo "  make install-tau-sdk         # install the Python SDK in the active Python"
	@echo "  make install-taugrid-portal  # install the TauGrid Portal CLI"
	@echo "  make uninstall-tau           # remove Tau CLI binaries"
	@echo "  make uninstall-taugrid-portal # remove the TauGrid Portal CLI"
	@echo "  make tau-docs-build          # build the documentation site"
	@echo "  make tau-docs-check          # build and validate the documentation site"
	@echo "  make tau-docs-serve          # serve the documentation site locally"
	@echo ""
	@echo "Override the Python SDK interpreter with PYTHON=/path/to/python."

build:
	$(MAKE) -C $(TAU_GO_DIR) build
	@echo "==> core"
	@cd $(CORE_DIR) && GOFLAGS="$(GO_COMMAND_FLAGS)" go build ./...
	$(MAKE) -C $(TAUGRID_PORTAL_DIR) build
	$(MAKE) -C $(TAU_CORE_CONTROLLER_DIR) build GOFLAGS="$(GO_COMMAND_FLAGS)"
	$(MAKE) -C $(GPU_HEALTH_CHECKER_DIR) build \
		GOARCH=$(HOST_GOARCH) GOFLAGS="$(GO_COMMAND_FLAGS) -trimpath"
	$(MAKE) -C $(GPU_METRICS_COLLECTOR_DIR) build GOFLAGS="$(GO_COMMAND_FLAGS)"

test:
	$(MAKE) -C $(TAU_GO_DIR) test
	@echo "==> core"
	@cd $(CORE_DIR) && go test ./...
	$(MAKE) -C $(TAUGRID_PORTAL_DIR) test
	$(MAKE) -C $(TAU_CORE_CONTROLLER_DIR) test
	$(MAKE) -C $(GPU_HEALTH_CHECKER_DIR) test GOARCH=$(HOST_GOARCH)
	$(MAKE) -C $(GPU_METRICS_COLLECTOR_DIR) test
	@echo "==> sdk/python/python"
	@cd $(TAU_SDK_DIR) && $(PYTHON) -m pytest

lint:
	$(MAKE) -C $(TAU_GO_DIR) lint GOFLAGS="$(GO_COMMAND_FLAGS)"
	@echo "==> core"
	@cd $(CORE_DIR) && GOFLAGS="$(GO_COMMAND_FLAGS)" go vet ./...
	@cd $(CORE_DIR) && gofmt -l . | tee /dev/stderr | (! read -r _)
	@cd $(CORE_DIR) && GOFLAGS="$(GO_COMMAND_FLAGS)" \
		go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...
	$(MAKE) -C $(TAUGRID_PORTAL_DIR) lint GOFLAGS="$(GO_COMMAND_FLAGS)"
	$(MAKE) -C $(TAU_CORE_CONTROLLER_DIR) lint GOFLAGS="$(GO_COMMAND_FLAGS)"
	$(MAKE) -C $(GPU_HEALTH_CHECKER_DIR) lint \
		GOARCH=$(HOST_GOARCH) GOFLAGS="$(GO_COMMAND_FLAGS) -trimpath"
	$(MAKE) -C $(GPU_METRICS_COLLECTOR_DIR) lint GOFLAGS="$(GO_COMMAND_FLAGS)"
	@echo "==> sdk/python/python"
	@cd $(TAU_SDK_DIR) && $(PYTHON) -m ruff check .

check:
	$(MAKE) build
	$(MAKE) test
	$(MAKE) lint
	$(MAKE) license-headers

license-headers:
	python3 $(REPO_ROOT)/scripts/check-license-headers.py

install-tau: install-tau-cli
	$(MAKE) -C $(TAU_SDK_DIR) install-optional PYTHON="$(PYTHON)"
	@echo ""
	@echo "Tau installed."
	@echo "  CLI: $$(command -v tau 2>/dev/null || echo 'not on PATH')"
	@sdk_version=$$("$(PYTHON)" -c 'import tau; print(tau.__version__)' 2>/dev/null || true); \
	if [ -n "$$sdk_version" ]; then \
		echo "  Python SDK: $$sdk_version"; \
	else \
		echo "  Python SDK: not installed (optional)"; \
	fi

define check_installed_on_path
	@bin_dir="$$(go env GOBIN 2>/dev/null || true)"; \
	[ -n "$$bin_dir" ] || bin_dir="$$(go env GOPATH 2>/dev/null || true)/bin"; \
	found="$$(command -v $(1) 2>/dev/null || true)"; \
	if [ -z "$$found" ]; then \
		echo "WARN: '$(1)' is not on PATH. Add $$bin_dir to PATH."; \
	elif [ "$$found" != "$$bin_dir/$(1)" ]; then \
		echo "WARN: PATH resolves '$(1)' to $$found, not $$bin_dir/$(1)."; \
	fi
endef

install-tau-cli:
	$(MAKE) -C $(TAU_GO_DIR) install
	$(call check_installed_on_path,tau)

install-tau-sdk:
	$(MAKE) -C $(TAU_SDK_DIR) install PYTHON="$(PYTHON)"

install-taugrid-portal:
	$(MAKE) -C $(TAUGRID_PORTAL_DIR) install
	$(call check_installed_on_path,taugrid-portal)

uninstall-tau:
	@bin_dir="$$(go env GOBIN)"; \
	[ -n "$$bin_dir" ] || bin_dir="$$(go env GOPATH)/bin"; \
	rm -f "$$bin_dir/tau" "$$bin_dir/tau-gen"; \
	echo "Removed Tau CLI binaries from $$bin_dir."

uninstall-taugrid-portal:
	@bin_dir="$$(go env GOBIN)"; \
	[ -n "$$bin_dir" ] || bin_dir="$$(go env GOPATH)/bin"; \
	rm -f "$$bin_dir/taugrid-portal"; \
	echo "Removed taugrid-portal from $$bin_dir."

tau-docs-build:
	$(MAKE) -C $(TAU_SITE_DIR) build

tau-docs-check:
	$(MAKE) -C $(TAU_SITE_DIR) check

tau-docs-serve:
	$(MAKE) -C $(TAU_SITE_DIR) serve
