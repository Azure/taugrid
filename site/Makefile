# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

HUGO_VERSION ?= v0.164.0
BIN_DIR := $(CURDIR)/.bin
HUGO := $(BIN_DIR)/hugo
CACHE_HOME ?= $(if $(XDG_CACHE_HOME),$(XDG_CACHE_HOME),$(HOME)/.cache)
HUGO_CACHEDIR ?= $(CACHE_HOME)/taugrid/hugo
GENERATED_DIR := $(CURDIR)/.generated
MARKET_POLICY_LOCAL := $(GENERATED_DIR)/tau-market-policy.cpu.json
MARKET_POLICY_REPEAT := $(GENERATED_DIR)/tau-market-policy.cpu.repeat.json
GOPROXY ?= https://proxy.golang.org,direct
BASE_URL ?=
BASE_PATH ?= /taugrid
HUGO_BASE_URL := $(if $(BASE_URL),--baseURL "$(BASE_URL)")

.PHONY: deps build check serve train-market-policy verify-cache-location verify-market-policy clean

deps: $(HUGO) node_modules

$(HUGO): go.mod
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 GOBIN=$(BIN_DIR) go install -tags extended github.com/gohugoio/hugo@$(HUGO_VERSION)

node_modules: package-lock.json
	npm ci
	@touch node_modules

build: verify-cache-location deps
	GOPROXY=$(GOPROXY) HUGO_CACHEDIR=$(HUGO_CACHEDIR) $(HUGO) --gc --minify $(HUGO_BASE_URL)

check: verify-cache-location build verify-market-policy
	python3 scripts/check_links.py public --base-path "$(BASE_PATH)"
	python3 scripts/check_content.py content/en/docs
	python3 scripts/check_wiki_parity.py
	python3 scripts/check_theme.py public
	python3 scripts/check_accessibility.py public assets/scss/_styles_project.scss
	cd ../cli && go test ./internal/cli -run TestDocumentedTauCommandsUsePublicTree -count=1

serve: verify-cache-location deps
	GOPROXY=$(GOPROXY) HUGO_CACHEDIR=$(HUGO_CACHEDIR) $(HUGO) server --buildDrafts --disableFastRender $(HUGO_BASE_URL)

train-market-policy:
	@mkdir -p $(GENERATED_DIR)
	MARKET_POLICY_REQUIRE_CUDA=0 python3 ../examples/market-policy/train.py \
		--device cpu \
		--output $(MARKET_POLICY_LOCAL)
	MARKET_POLICY_REQUIRE_CUDA=0 python3 ../examples/market-policy/train.py \
		--device cpu \
		--output $(MARKET_POLICY_REPEAT)
	cmp $(MARKET_POLICY_LOCAL) $(MARKET_POLICY_REPEAT)
	@rm -f $(MARKET_POLICY_REPEAT)
	@echo "deterministic CPU artifact: $(MARKET_POLICY_LOCAL)"

verify-cache-location:
	@case "$(abspath $(HUGO_CACHEDIR))/" in "$(CURDIR)"/*) \
		echo "HUGO_CACHEDIR must be outside the worktree: $(HUGO_CACHEDIR)" >&2; \
		exit 1;; \
	esac

verify-market-policy:
	python3 scripts/check_market_policy.py static/models/tau-market-policy.json

clean:
	rm -rf public resources .generated
