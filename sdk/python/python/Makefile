# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

.PHONY: install install-optional install-from-git require-usable-python python-env-guidance vendor-snippet test clean help

TAU_PY_DIR := $(abspath $(dir $(firstword $(MAKEFILE_LIST))))

PYTHON ?= python3

PYTHON_ENV_OK := $(PYTHON) -c 'import os, sys, sysconfig; sys.exit(0 if sys.prefix != sys.base_prefix or not os.path.exists(os.path.join(sysconfig.get_path("stdlib"), "EXTERNALLY-MANAGED")) else 1)'

REF ?= v0.1.0
GIT_REPO := ssh://git@github.com/Azure/taugrid.git

help:
	@echo "Tau Python SDK install targets:"
	@echo ""
	@echo "  make install              # editable install in the ACTIVE python (venv recommended)"
	@echo "  make install-from-git     # editable install pointing at this checkout (any venv)"
	@echo "  make vendor-snippet       # print pip/uv one-liners for vendoring from GitHub"
	@echo "  make test                 # run pytest"
	@echo ""
	@echo "Most common: cd into your project venv, then 'make -C $(TAU_PY_DIR) install-from-git'"
	@echo ""
	@echo "Override the target interpreter with PYTHON=, e.g."
	@echo "  make install PYTHON=~/.venvs/tau/bin/python"

python-env-guidance:
	@echo ""
	@echo "The Tau Python SDK cannot be installed into this interpreter:"
	@echo "  $$($(PYTHON) -c 'import sys; print(sys.executable)' 2>/dev/null || echo '$(PYTHON)')"
	@echo "It is externally managed (PEP 668) and is not a virtualenv, so pip will"
	@echo "refuse the install."
	@echo ""
	@echo "Install it with the pip inside a virtualenv:"
	@echo "  python3 -m venv ~/.venvs/tau"
	@echo "  source ~/.venvs/tau/bin/activate"
	@echo "  make -C $(TAU_PY_DIR) install"
	@echo ""
	@echo "Or point the install at your own interpreter without activating it:"
	@echo "  make -C $(TAU_PY_DIR) install PYTHON=~/.venvs/tau/bin/python"
	@echo "  ~/.venvs/tau/bin/python -m pip install -e '$(TAU_PY_DIR)[dev]'"
	@echo ""
	@echo "'tau python' resolves python3 from PATH, so activate the venv before using it."
	@echo "Avoid --break-system-packages: it writes into the apt-managed Python."
	@echo ""

install: require-usable-python
	$(PYTHON) -m pip install -e .[dev]

install-optional:
	@if $(PYTHON_ENV_OK); then \
	  $(MAKE) --no-print-directory install; \
	else \
	  $(MAKE) --no-print-directory python-env-guidance; \
	  echo "Skipping the Tau Python SDK — it is optional and the Go CLI does not need it."; \
	fi

install-from-git: require-usable-python
	$(PYTHON) -m pip install -e "$(TAU_PY_DIR)[dev]"

require-usable-python:
	@$(PYTHON_ENV_OK) || { $(MAKE) --no-print-directory python-env-guidance; exit 1; }

vendor-snippet:
	@echo "# Vendor the Tau Python SDK from GitHub (package name tau, pinned to $(REF)):"
	@echo ""
	@echo "# pip:"
	@echo "pip install \"tau @ git+$(GIT_REPO)@$(REF)#subdirectory=sdk/python\""
	@echo ""
	@echo "# uv (in pyproject.toml):"
	@echo "[tool.uv.sources]"
	@echo "tau = { git = \"$(GIT_REPO)\", rev = \"$(REF)\", subdirectory = \"sdk/python\" }"
	@echo ""
	@echo "# Then add the dependency:"
	@echo "uv add tau"
	@echo ""
	@echo "Override with:  make vendor-snippet REF=<sha-or-tag-or-branch>"

test:
	pytest tests/

clean:
	rm -rf build dist *.egg-info .pytest_cache
	find . -type d -name __pycache__ -exec rm -rf {} +
