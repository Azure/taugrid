# Optional Python SDK

Use the SDK when the project already uses `@tau.train`, `@tau.eval`, or
`tau.serve`, or explicitly asks for Python-first authoring. Ordinary direct
`tau.yaml` workloads do not need it.

`tau python` proxies to `python3 -m tau.cli` in the active environment.
The Python SDK and Go CLI are independently versioned; inspect both before
assuming matching options. Install/upgrade only when requested.

## Inspect and build before submitting

```bash
tau python --help
tau python inspect train.py
tau python build train.py --output build/train
```

`inspect` imports the module and prints generated manifests; `build` imports
it and writes a deterministic staged artifact. Review untrusted Python first:
neither command is a sandbox, and top-level module code can have side effects.
Keep actual submission behind `if __name__ == "__main__":`.
Do not use build's `--force` without reviewing the existing output.

For local API exploration, inspect signatures with the installed package.
Use `tau.config(...)` to compose project YAML/TOML with decorator options.
`@tau.train` covers pretraining/fine-tuning/post-training. `@tau.eval` creates
a managed evaluation shape with a GPU worker and CPU fanout workers, rather
than the ordinary direct-eval config described in the main skill.

Generated `schema_version: 1` manifests and staged wrappers belong to the
managed workflow contract. `tau run validate` explicitly excludes that
contract. Do not copy its `compute.cpus`, `compute.memory`, `eval`, or other
managed fields into a direct run config.

## Submission is still Go-backed

```bash
tau python submit train.py --dry-run=client
tau python submit-build build/train --dry-run=client
```

These invoke Go Tau for profile resolution/rendering and inherit its connected
preconditions. Client dry-run skips polling and chained eval submission; it
does not prove the whole pipeline ran. Normal chained submission can wait for
training, delete completed RayJobs to release GPUs, and submit eval. Obtain
authorization for that lifecycle, not just module inspection.

`tau python doctor` checks CLI/SDK/kubectl prerequisites and can perform cluster
checks; do not label it offline.

## Watch for SDK/CLI compatibility gaps

The SDK can retain options longer than the Go CLI:

- Training submission's legacy `preset=` writes `policy.preset`, which the
  current Go config rejects. Use the managed config's `policy.profile` and
  inspect the generated artifact; do not rely on the old keyword.
- `tau.serve(..., profiles_dir=...)` forwards removed `--profiles-dir`.
  Select a ready TauCluster profile by name instead.
- SDK serving `args=` uses legacy `--args`; a Python list does not guarantee
  literal argument preservation through the Go parser. For Deployment argv
  containing spaces/commas, prefer direct CLI `--command`/`--arg`, or explicitly
  pass verified CLI arguments through `extra_args`.

Report a compatibility gap as such; do not silently patch installed packages
or promise support from a Python signature alone.

## Secrets, serving, and evidence

Prefer `tau.secret_ref(...)` / Secret references for workload configuration.
Never print or check in resolved secret payloads. Treat generated build
artifacts as potentially sensitive until inspected.

`tau.serve(...)` delegates to `tau serve deploy`; workspace targeting,
connected dry-run, checkpoint lookup, and profile checks still apply.
It does not turn a non-Ray entrypoint into a Ray Serve app. Read
[Serving](serving.md) for the current CLI contract.

Use `tau.stellar` for application metric logging where the project supports
it, and the separate `taugrid-portal` binary for the evidence UI. A generated
manifest or successful import proves neither a Kubernetes run nor durable
metric delivery.
