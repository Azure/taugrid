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
make install-tau
tau version
```

`make install-tau` installs the CLI and installs the Python SDK into the active
Python environment. Activate a project virtual environment first when you want
the SDK isolated to that project.

The Python SDK is optional — only `tau python` and the `@tau.train()` decorator
need it. On Debian and Ubuntu (Python 3.11 and later) the system interpreter is
marked externally managed under [PEP 668](https://peps.python.org/pep-0668/) and
pip refuses to install into it. `make install-tau` reports that as a skip and
still installs the CLI. To install the SDK, use a virtual environment:

```bash
python3 -m venv ~/.venvs/tau
source ~/.venvs/tau/bin/activate
make install-tau-sdk
```

Or target an interpreter without activating it:

```bash
make install-tau-sdk PYTHON=~/.venvs/tau/bin/python
```

`tau python` resolves `python3` from `PATH`, so activate the virtual environment
before using it.

## Install the released CLI

Tau releases publish `tau-{darwin,linux}-{amd64,arm64}`,
`tau-gen-{darwin,linux}-{amd64,arm64}` (the standalone repo scaffold
generator), and `SHA256SUMS`. Download what you need plus the checksum file from
the same
[GitHub Release](https://github.com/Azure/taugrid/releases),
verify the checksum, and install the binary on `PATH`.

The Python SDK is installed from the same tagged source revision; releases do
not currently contain Python wheels.

See the
[versioned installation reference](https://github.com/Azure/taugrid/blob/main/docs/install-tau.md)
for exact release download and SDK commands.
