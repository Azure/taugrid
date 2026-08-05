# Contributing to TauGrid

For people changing this repository's Go code. Users of the `tau` binary do not
need any of this.

## Module layout

Three Go modules, split so the researcher CLI does not link the portal's web
surface (~39k lines of embedded assets, SQLite/Parquet store, and Kusto stack
that only the long-running servers need).

Cross-module `internal/` imports are illegal in Go, so shared code sits at the
**root** of `core`, not under `internal/`.

| Path | Contents |
|---|---|
| `cli` | The `tau` CLI: `internal/cli`, renderers, run lifecycle |
| `core` | Shared: `status`, `queue`, `runconfig`, `topology`, `resourceprofile`, `workloadmeta`, `kube`, `version` |
| `portal` | Stellar + Portal: `expstore`, `expcockpit`, `portalapi`, `autocapture` |
| `sdk/python` | Python SDK; delegates execution to the Go CLI |
| `site/content/en/docs` | Hugo/Docsy user documentation |

There is no `go.work`. Both consumer modules use `require` + `replace
../taucore`, so they are not independently `go get`-able — fine, since they are
never published.

## CI guards you can trip

| Guard | Fails when |
|---|---|
| `command_tree_test.go` | The public root set changes, or `experiment`/`portal` reappear in `tau` |
| `staticcheck` | Unreferenced code, or any other enabled check; see `staticcheck.conf` at the repo root |
| `TestNoInlineTauKeyLiterals` | Any file outside `workloadmeta` writes a `tau.azure.com/*` key as a string literal |

`workloadmeta` declares every `tau.azure.com/*` label, annotation, and
finalizer exactly once. That guard exists because of real incidents: a README
documented a key long after the code emitted a different one, so every
documented `kubectl` command matched zero pods; and an admission policy kept
matching a key the CLI had stopped emitting, silently disabling the policy with
no error. Import the constant — never retype the string, and never derive one
key from another by prefix matching (`LabelProfile` and the `profile-*`
profiler annotations share a prefix by accident and are unrelated contracts).

## Verifying a change

Compilation is not proof a feature works. Rebuild and run the real command:

```bash
make install-tau-cli          # or make install-taugrid-portal
tau <the subcommand you changed>
```

For runtime features (multi-node rendezvous, GPU placement, storage mounts), a
dry-run render is still not proof — those fail in ways only a real cluster
surfaces. Submit an actual job.

## Editing this skill

Re-run the example checker after touching any YAML block. A config that does
not validate gets copy-pasted and fails on the user's first attempt:

```bash
python3 skills/taugrid/scripts/check_examples.py
```

Both `SKILL.md` and the references state contracts that the CLI enforces. When
they disagree with the implementation, the implementation wins — verify with
`tau run schema -o json` and fix the skill.

`site/` docs are not automatically correct either, and the fix belongs there
rather than as a permanent divergence in this skill. The `StorageReady` claim
was corrected in `site/` alongside this skill; if you find another mismatch,
verify it against source first — a "known doc bug" repeated from memory is how
the wrong version spreads.
