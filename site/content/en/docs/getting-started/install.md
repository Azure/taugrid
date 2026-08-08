---
title: Install Tau
weight: 1
description: Install the Tau CLI and optional Python SDK
---

{{< maturity status="shipped" reviewed="2026-07-16" >}}

Tau consists of the Go `tau` CLI and an optional Python SDK package also
imported as `tau`. The CLI is the canonical Kubernetes executor.

## Install from a source checkout

```bash
git clone https://github.com/Azure/taugrid.git
cd taugrid
make -C cli install
tau version
```

This installs `tau` and `tau-gen` into the active Go binary directory. Ensure
that directory is on `PATH`.

The Python SDK is optional — only `tau python` and the `@tau.train()` decorator
need it. On Debian and Ubuntu (Python 3.11 and later) the system interpreter is
marked externally managed under [PEP 668](https://peps.python.org/pep-0668/) and
pip refuses to install into it. `make install-tau` reports that as a skip and
still installs the CLI. To install the SDK, use a virtual environment:

```bash
python3 -m venv ~/.venvs/tau
source ~/.venvs/tau/bin/activate
make -C sdk/python/python install
```

Or target an interpreter without activating it:

```bash
make -C sdk/python/python install PYTHON=~/.venvs/tau/bin/python
```

`tau python` resolves `python3` from `PATH`, so activate the virtual environment
before using it.

## Install the released CLI

On Linux and macOS (amd64 or arm64), install the latest release without sudo:

```bash
curl -fsSL https://github.com/Azure/taugrid/releases/latest/download/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
tau version --short
```

The installer detects the current platform, downloads the matching binary,
verifies it against the release's `SHA256SUMS`, and installs it to
`$HOME/.local/bin`. It installs the MIT license to
`$HOME/.local/share/doc/tau/LICENSE`. Override these destinations with
`TAU_INSTALL_DIR` and `TAU_LICENSE_DIR`.

To install a specific release:

```bash
curl -fsSL https://github.com/Azure/taugrid/releases/download/v0.1.3/install.sh |
  TAU_VERSION=v0.1.3 sh
```

Tau releases publish `tau-{darwin,linux}-{amd64,arm64}`,
`tau-gen-{darwin,linux}-{amd64,arm64}` (the standalone repo scaffold
generator), `install.sh`, `LICENSE`, and `SHA256SUMS`. You can also download
what you need plus the checksum file from the same
[GitHub Release](https://github.com/Azure/taugrid/releases),
verify the checksum, and install the binary on `PATH`.

The Python SDK is installed from the same tagged source revision; releases do
not currently contain Python wheels.

See [`cli/RELEASING.md`](https://github.com/Azure/taugrid/blob/main/cli/RELEASING.md)
for the maintainer release checklist.
