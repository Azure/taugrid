# taugrid-portal

`taugrid-portal` is the Tau experiment and observability web surface. It was
split out of the `tau` CLI so that submitting a workload no longer links an
embedded web frontend, a SQLite-backed experiment store, and a Kusto query
stack into the same binary a researcher runs from a laptop.

The verb surface is unchanged. Everything that was `taugrid-portal experiment ...` or
`taugrid-portal portal ...` is now `taugrid-portal experiment ...` and
`taugrid-portal portal ...`, with the same flags, output, and behaviour. The
hidden `exp` alias is preserved.

## Commands

| Command | Responsibility |
| --- | --- |
| `taugrid-portal experiment` | Track, import, query, compare, and visualize experiment state through expstore and Stellar. |
| `taugrid-portal portal` | Serve the read-only cross-system observability portal. |
| `taugrid-portal version` | Print build and version information. |

## Layout

| Path | Contents |
| --- | --- |
| `internal/expstore` | The local-first experiment store: run packets, metrics, artifacts, search index. |
| `internal/expcockpit` | Stellar: snapshot construction and the embedded HTML/CSS/JS frontend. |
| `internal/expapi` | The HTTP server behind `experiment serve` and the mounted Stellar experience. |
| `internal/expimport` | TensorBoard, Weights & Biases, and JSONL metric importers. |
| `internal/expcapture` | Maps a `core/status` run profile into experiment store records. |
| `internal/autocapture` | Reconciles Kubernetes Job and RayJob state into experiment runs. |
| `internal/portalapi`, `internal/portal/*` | The portal HTTP surface and its per-board data sources. |
| `internal/artifactoffload`, `internal/blobstore`, `internal/jsonlutil` | Durable artifact upload and the JSONL plumbing that feeds it. |

Shared contracts — run status, queues, topology, run config, experiment
identity, workload labels — live in
[`../core`](../core) and are imported by both this binary and the tau
CLI.

## Stable boundaries

- Local expstore remains authoritative for run packets, artifacts, checkpoints,
  and recovery state. ADX/Kusto is a downstream analytics projection only, not
  the source of truth, and telemetry is not the only copy of non-scalar run
  state.
- The portal and Stellar are read-only. They never mutate cluster state.
- Secret values belong in Kubernetes Secret or Key Vault references, never in
  checked-in configs, ConfigMaps, annotations, logs, metrics, or screenshots.

## Build

```bash
cd portal
go build ./...
go test ./...
make build          # writes ./bin/taugrid-portal with version ldflags
```

The container image is built from [`../../images/taugrid-portal`](../../images/taugrid-portal).
