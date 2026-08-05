package storageprobe

import "strings"

const preflightScript = `tau_warn() { echo "tau storage warning: $*" >&2; }
: "${TAU_DATA_DIR:=/data}"
: "${TAU_HOT_DIR:=/mnt}"
: "${TAU_DURABLE_DATASETS_DIR:=$TAU_DATA_DIR/datasets}"
: "${TAU_DURABLE_CHECKPOINTS_DIR:=$TAU_DATA_DIR/checkpoints}"
hot_ok=1
hot_reason=""
hot_probe_mbps=""
hot_required_kib="${TAU_HOT_REQUIRED_KIB:-1048576}"
hot_probe_mib="${TAU_HOT_PROBE_MIB:-64}"
hot_min_write_mbps="${TAU_HOT_MIN_WRITE_MBPS:-50}"
if [ ! -d "$TAU_HOT_DIR" ]; then
  hot_ok=0; hot_reason="missing"
elif ! mkdir -p "$TAU_HOT_DIR/datasets" "$TAU_HOT_DIR/checkpoints" 2>/dev/null; then
  hot_ok=0; hot_reason="mkdir failed"
else
  hot_avail=""
  if command -v df >/dev/null 2>&1; then
    hot_avail="$(df -Pk "$TAU_HOT_DIR" 2>/dev/null | {
      IFS= read -r _ || true
      IFS= read -r _ _ _ avail _ || true
      printf '%s' "$avail"
    })"
  else
    tau_warn "df not found; skipping free-space check for $TAU_HOT_DIR"
  fi
  if [ -n "$hot_avail" ] && [ "$hot_avail" -le "$hot_required_kib" ]; then
    hot_ok=0; hot_reason="insufficient free space (${hot_avail:-unknown} KiB < ${hot_required_kib} KiB)"
  else
    if command -v python3 >/dev/null 2>&1; then
      hot_probe_result="$(python3 - <<'PY'
import os
import sys
import time

root = os.environ["TAU_HOT_DIR"]
mib = int(os.environ.get("TAU_HOT_PROBE_MIB", "64"))
minimum = float(os.environ.get("TAU_HOT_MIN_WRITE_MBPS", "50"))
path = os.path.join(root, ".tau-write-probe.%d" % os.getpid())
buf = b"\0" * (1024 * 1024)
start = time.monotonic()
try:
    with open(path, "wb", buffering=0) as f:
        for _ in range(mib):
            f.write(buf)
        os.fsync(f.fileno())
    dir_fd = os.open(root, os.O_RDONLY)
    try:
        os.fsync(dir_fd)
    finally:
        os.close(dir_fd)
except Exception as exc:
    print("failed:%s" % exc)
    sys.exit(2)
finally:
    try:
        os.remove(path)
    except FileNotFoundError:
        pass
elapsed = max(time.monotonic() - start, 0.001)
mbps = mib / elapsed
if mbps < minimum:
    print("slow:%.3f" % mbps)
    sys.exit(3)
print("ok:%.3f" % mbps)
PY
)" || hot_probe_result="${hot_probe_result:-failed}"
      case "$hot_probe_result" in
        ok:*) hot_probe_mbps="${hot_probe_result#ok:}" ;;
        slow:*) hot_probe_mbps="${hot_probe_result#slow:}"; hot_ok=0; hot_reason="write probe slow (${hot_probe_mbps} MB/s < ${hot_min_write_mbps} MB/s)" ;;
        failed:*) hot_ok=0; hot_reason="write probe ${hot_probe_result#failed:}" ;;
        *) hot_ok=0; hot_reason="write probe failed" ;;
      esac
    else
      hot_probe="$TAU_HOT_DIR/.tau-write-probe.$$"
      if ! ( : > "$hot_probe" ) 2>/dev/null; then
        hot_ok=0; hot_reason="write probe failed"
      fi
      rm -f "$hot_probe" 2>/dev/null || true
      hot_probe_mbps="unknown"
    fi
  fi
fi
if [ "$hot_ok" = "1" ]; then
  export TAU_DATASETS_DIR="${TAU_DATASETS_DIR:-$TAU_HOT_DIR/datasets}"
  export TAU_CHECKPOINTS_DIR="${TAU_CHECKPOINTS_DIR:-$TAU_HOT_DIR/checkpoints}"
  export TAU_STORAGE_HOT_STATUS="hot"
  export TAU_STORAGE_HOT_REASON=""
else
  tau_warn "$TAU_HOT_DIR is $hot_reason; falling back to durable $TAU_DATA_DIR for hot datasets/checkpoints"
  mkdir -p "$TAU_DURABLE_DATASETS_DIR" "$TAU_DURABLE_CHECKPOINTS_DIR"
  export TAU_HOT_DIR="$TAU_DATA_DIR"
  export TAU_DATASETS_DIR="$TAU_DURABLE_DATASETS_DIR"
  export TAU_CHECKPOINTS_DIR="$TAU_DURABLE_CHECKPOINTS_DIR"
  export TAU_STORAGE_HOT_STATUS="durable-fallback"
  export TAU_STORAGE_HOT_REASON="$hot_reason"
fi
export TAU_STORAGE_HOT_WRITE_MBPS="$hot_probe_mbps"
export TAU_DATA_DIR TAU_DURABLE_DATASETS_DIR TAU_DURABLE_CHECKPOINTS_DIR
`

func Script() string {
	return preflightScript
}

func IndentedScript(spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.TrimSuffix(preflightScript, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = prefix
		} else {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
