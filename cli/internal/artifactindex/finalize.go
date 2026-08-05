// Package artifactindex emits the durable artifact index (artifacts.json) that
// the CLI's train -> serve handoff depends on.
//
// Why this exists: `tau serve deploy --from-finetune/--checkpoint-ref` and
// `tau data model index rebuild` both resolve a trained checkpoint by reading
// /data/checkpoints/finetunes/<run>/artifacts.json. Until this package, the
// only writer of that file in the repository was the Python SDK's cluster
// wrapper (sdk/python/tau/_cluster.py, _finalize_train_artifacts). Managed
// workflows launched by `tau run` execute the researcher's script directly and
// never load that wrapper, so a CLI-only researcher could train a model and
// then had no supported way to serve it — every registry-aware path failed to
// resolve the run, leaving `--checkpoint <absolute path>` typed by hand as the
// only escape.
//
// The emitted schema matches managedWorkflowArtifactIndex in
// cli/internal/cli/pvc_helpers.go. Keep the two in sync; finalize_test.go
// asserts the emitted field names agree with that struct's JSON tags.
package artifactindex

import "strings"

// Config carries the render-time values the finalize step needs. These are
// passed explicitly rather than read from the pod environment because the
// managed-workflow entrypoint resolves ${NAME}-style markers at render time,
// not at runtime — there is no TAU_RUN_NAME env var to read.
type Config struct {
	// Artifact is the manifest's artifacts.checkpoint value, e.g.
	// "last.safetensors". Empty means the manifest declared no checkpoint.
	Artifact string
	// Run is the run name; it names the durable directory
	// <durable>/finetunes/<Run>/ that artifacts.json is written into.
	Run string
	// ResourceName and Namespace are recorded in the index for provenance.
	ResourceName string
	Namespace    string
}

// Script returns a POSIX shell snippet that finalizes the declared checkpoint
// artifact and writes artifacts.json.
//
// It exits 0 when the declared artifact is simply absent, so a training run
// that produced real results is not failed over a checkpoint the researcher's
// own script did not write. It exits 126 when the index cannot be written at
// all: no python3 in the image, a refused destination, or an I/O failure. A
// run that declared storage.checkpoint and has no artifact index is not
// servable, and reporting success for it hides that until deploy time.
//
// Callers run it only after training succeeds — the entrypoint's `set -eu`
// guarantees a failed training run never reaches it.
//
// An empty Artifact or Run yields an empty script, so manifests that declare no
// checkpoint render exactly as they did before this step existed.
func Script(cfg Config) string {
	if strings.TrimSpace(cfg.Artifact) == "" || strings.TrimSpace(cfg.Run) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("TAU_ARTIFACT_CHECKPOINT=" + shellQuote(cfg.Artifact) + "\n")
	b.WriteString("TAU_ARTIFACT_RUN=" + shellQuote(cfg.Run) + "\n")
	b.WriteString("TAU_ARTIFACT_RESOURCE=" + shellQuote(cfg.ResourceName) + "\n")
	b.WriteString("TAU_ARTIFACT_NAMESPACE=" + shellQuote(cfg.Namespace) + "\n")
	b.WriteString("export TAU_ARTIFACT_CHECKPOINT TAU_ARTIFACT_RUN TAU_ARTIFACT_RESOURCE TAU_ARTIFACT_NAMESPACE\n")
	b.WriteString(finalizeScript)
	return b.String()
}

// shellQuote wraps s in single quotes, escaping any embedded single quote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// finalizeScript mirrors _finalize_train_artifacts in the Python SDK: locate the
// declared artifact across the same candidate roots, copy it into the durable
// artifacts directory, then write the index atomically.
//
// It reads its inputs from the TAU_ARTIFACT_* variables that Script exports
// immediately above it, so this constant is identical for every run and the
// only render-time quoting happens in Script.
const finalizeScript = `# Emit the durable artifact index that 'tau serve --from-finetune' and
# 'tau data model index rebuild' read. Best-effort for transient faults: a
# training run that produced real results is not failed over bookkeeping.
#
# A missing interpreter is not a transient fault. It is a static property of
# the image, so every retry writes the same nothing, and reporting success
# would tell the researcher a declared storage.checkpoint was honoured when no
# index exists. That only surfaces much later, when serve cannot resolve the
# run. Refuse here instead, using the same exit code the sibling
# artifactpublish package uses for "refused after the payload succeeded".
if ! command -v python3 >/dev/null 2>&1; then
  echo "tau: cannot write the artifact index: this image has no python3." >&2
  echo "tau: storage.checkpoint '$TAU_ARTIFACT_CHECKPOINT' was declared, so this run" >&2
  echo "tau: is not resolvable by 'tau serve deploy --from-finetune $TAU_ARTIFACT_RUN'." >&2
  echo "tau: Use an image that ships python3, or drop storage.checkpoint and pass" >&2
  echo "tau: an absolute --checkpoint path to serve." >&2
  exit 126
fi
# The program below exits 0 for the one benign outcome — the declared artifact
# simply is not there, which is the run's own doing and not worth failing over.
# Every other exit is non-zero and fails the run: refusing an unsafe
# destination, and also any I/O error or unexpected exception (a read-only
# durable dir, a failed mkdir, a stat that blows up on a flaky mount). That
# second group is deliberately included. Collapsing it into a warning is what
# let a refusal to write into another run's directory, and an index that was
# never written at all, both report success.
if ! python3 - <<'TAU_ARTIFACT_INDEX_EOF'
import datetime, json, os, pathlib, shutil, sys

artifact = os.environ.get("TAU_ARTIFACT_CHECKPOINT", "").strip()
run = os.environ.get("TAU_ARTIFACT_RUN", "").strip()
if not artifact or not run:
    sys.exit(0)

hot = pathlib.Path(os.environ.get("TAU_CHECKPOINTS_DIR", "/mnt/checkpoints"))
durable = pathlib.Path(os.environ.get("TAU_DURABLE_CHECKPOINTS_DIR", "/data/checkpoints"))
rel = pathlib.PurePosixPath(artifact)

# Defense in depth. tau validates storage.checkpoint before it ever reaches this
# script, but this PVC is shared by every run in the namespace, so a traversal
# here is a write into another researcher's run rather than a local mistake. An
# absolute value is the sharp edge: pathlib drops every component before it, so
# run_dir / "artifacts" / "/etc/passwd" is exactly "/etc/passwd". Exiting
# non-zero refuses the copy, and the wrapper above surfaces that as a failed
# run: a refusal is not the same outcome as a written index, and a run that
# reports success while its declared checkpoint went unindexed is the failure
# mode this whole step exists to prevent.
if rel.is_absolute() or any(part in ("", ".", "..") for part in rel.parts):
    print("tau: refusing unsafe checkpoint artifact path " + repr(artifact) +
          " (must be relative, without '.' or '..' segments)", file=sys.stderr)
    sys.exit(1)

# Same candidate order as the SDK wrapper (_artifact_sources).
candidates = [
    hot.joinpath(*rel.parts),
    hot / "finetunes" / run / rel,
    durable / run / rel,
    durable / "finetunes" / run / rel,
    durable / "finetunes" / run / "artifacts" / rel,
]
seen, ordered = set(), []
for c in candidates:
    if c.as_posix() not in seen:
        seen.add(c.as_posix())
        ordered.append(c)

src = next((c for c in ordered if c.exists()), None)
if src is None:
    tried = ", ".join(c.as_posix() for c in ordered)
    print("tau: declared checkpoint artifact " + repr(artifact) +
          " not found after training. Tried: " + tried, file=sys.stderr)
    sys.exit(0)

run_dir = durable / "finetunes" / run
dst = run_dir / "artifacts" / rel
dst.parent.mkdir(parents=True, exist_ok=True)

# The segment check above is syntactic; this one survives symlinks. A symlink
# planted under the shared PVC can still point outside the run directory after
# the parts look clean, so verify where the path actually lands before writing.
resolved_root = run_dir.resolve()
if not str(dst.resolve()).startswith(str(resolved_root) + os.sep):
    print("tau: refusing checkpoint artifact destination outside the run directory: " +
          dst.as_posix(), file=sys.stderr)
    sys.exit(1)

started = datetime.datetime.now(datetime.timezone.utc)
if src.resolve() != dst.resolve():
    if src.is_dir():
        # Data-only copy. Azure Files (SMB) rejects the utime call that
        # metadata-preserving copies make, with EPERM for a non-owner, which
        # would abort an otherwise successful run.
        shutil.copytree(src, dst, dirs_exist_ok=True, copy_function=shutil.copyfile)
    else:
        shutil.copyfile(src, dst)
completed = datetime.datetime.now(datetime.timezone.utc)

if dst.is_dir():
    files = [p for p in dst.rglob("*") if p.is_file()]
    size, count, kind = sum(p.stat().st_size for p in files), len(files), "directory"
else:
    size, count, kind = dst.stat().st_size, 1, "file"

record = {
    "name": "checkpoint",
    "manifest_path": artifact,
    "source_path": src.as_posix(),
    "durable_path": dst.as_posix(),
    "status": "ready",
    "kind": kind,
    "size_bytes": size,
    "file_count": count,
    "mtime": datetime.datetime.fromtimestamp(
        dst.stat().st_mtime, datetime.timezone.utc).isoformat(),
    "upload_started_at": started.isoformat(),
    "upload_completed_at": completed.isoformat(),
    "upload_duration_ms": int((completed - started).total_seconds() * 1000),
}
index = {
    "schema_version": 1,
    "run": run,
    "namespace": os.environ.get("TAU_ARTIFACT_NAMESPACE", ""),
    "resource_name": os.environ.get("TAU_ARTIFACT_RESOURCE", ""),
    "created_at": completed.isoformat(),
    "hot_root": hot.as_posix(),
    "durable_root": durable.as_posix(),
    "artifacts": [record],
}

index_path = run_dir / "artifacts.json"
tmp = index_path.with_suffix(".json.tmp")
tmp.parent.mkdir(parents=True, exist_ok=True)
tmp.write_text(json.dumps(index, indent=2, sort_keys=True) + "\n")
os.replace(tmp, index_path)
print("tau: wrote artifact index " + index_path.as_posix() +
      " (" + dst.as_posix() + ", " + str(size) + " bytes)", file=sys.stderr)
TAU_ARTIFACT_INDEX_EOF
then
  echo "tau: artifact index step failed; storage.checkpoint '$TAU_ARTIFACT_CHECKPOINT' is not indexed" >&2
  exit 126
fi`

// IndentedScript returns Script(cfg) with every line prefixed by spaces, for
// embedding in a YAML block scalar. Mirrors storageprobe.IndentedScript.
func IndentedScript(cfg Config, spaces int) string {
	body := Script(cfg)
	if body == "" {
		return ""
	}
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = prefix
			continue
		}
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// WrapCommand returns cmd wrapped so the artifact index step runs after it
// succeeds. It is the container-command analogue of Script, for the renderers
// that build an argv rather than a shell entrypoint (jobrender, rayjobrender).
//
// The wrapper runs the original command under `set -e`, so a non-zero exit
// propagates and the index step is skipped — an artifact index must never
// claim a model that a failed run did not produce.
//
// Returns cmd unchanged when there is nothing to index, so runs that declare
// no checkpoint render exactly as they did before.
func WrapCommand(cmd []string, cfg Config) []string {
	body := Script(cfg)
	if len(cmd) == 0 || body == "" {
		return cmd
	}
	wrapper := "set -e\n\"$@\"\n" + body
	return append([]string{"bash", "-lc", wrapper, "tau-entrypoint"}, cmd...)
}
