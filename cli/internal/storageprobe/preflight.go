// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storageprobe

import "strings"

const preflightScript = `tau_warn() { echo "tau storage warning: $*" >&2; }
tau_mount_of() {
  tau_m_dev=""; tau_m_point=""; tau_m_fstype=""; tau_m_source=""
  [ -r "$tau_mountinfo" ] || return 0
  while IFS=' ' read -r _ _ tau_mi_dev _ tau_mi_point _ tau_mi_rest; do
    case "$1" in
      "$tau_mi_point"|"$tau_mi_point"/*) ;;
      *) [ "$tau_mi_point" = "/" ] || continue ;;
    esac
    [ "${#tau_mi_point}" -ge "${#tau_m_point}" ] || continue
    tau_mi_tail=" $tau_mi_rest"
    tau_mi_tail="${tau_mi_tail#* - }"
    tau_m_dev="$tau_mi_dev"
    tau_m_point="$tau_mi_point"
    tau_m_fstype="${tau_mi_tail%% *}"
    tau_mi_tail="${tau_mi_tail#* }"
    tau_m_source="${tau_mi_tail%% *}"
  done < "$tau_mountinfo"
}
tau_mountinfo="${TAU_HOT_MOUNTINFO:-/proc/self/mountinfo}"
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
hot_backing=""
if [ ! -d "$TAU_HOT_DIR" ]; then
  hot_ok=0; hot_reason="missing"
elif ! mkdir -p "$TAU_HOT_DIR/datasets" "$TAU_HOT_DIR/checkpoints" 2>/dev/null; then
  hot_ok=0; hot_reason="not writable"
else
  hot_real="$(cd "$TAU_HOT_DIR" 2>/dev/null && pwd -P)" || hot_real="$TAU_HOT_DIR"
  tau_mount_of "$hot_real"
  hot_dev="$tau_m_dev"; hot_source="$tau_m_source"; hot_fstype="$tau_m_fstype"
  tau_mount_of /
  hot_root_dev="$tau_m_dev"
  tau_mount_of /etc/hosts
  hot_node_dev=""
  if [ "$tau_m_point" = "/etc/hosts" ]; then hot_node_dev="$tau_m_dev"; fi
  if [ -z "$hot_dev" ]; then
    hot_backing="unknown"
  elif [ "$hot_dev" = "$hot_root_dev" ]; then
    hot_backing="container-root"
  elif [ -n "$hot_node_dev" ] && [ "$hot_dev" = "$hot_node_dev" ]; then
    hot_backing="kubelet-disk"
  else
    hot_backing="dedicated"
  fi
  hot_size=""
  hot_avail=""
  if command -v df >/dev/null 2>&1; then
    hot_df="$(df -Pk "$TAU_HOT_DIR" 2>/dev/null | {
      IFS= read -r _ || true
      read -r _ size _ avail _ || true
      printf '%s %s' "$size" "$avail"
    })"
    hot_size="${hot_df%% *}"
    hot_avail="${hot_df#* }"
  else
    tau_warn "df not found; skipping free-space check for $TAU_HOT_DIR"
  fi
  echo "tau storage: $TAU_HOT_DIR source=${hot_source:-unknown} fstype=${hot_fstype:-unknown} size_kib=${hot_size:-unknown} avail_kib=${hot_avail:-unknown} backing=$hot_backing" >&2
  if [ "${TAU_HOT_REQUIRE_LOCAL:-}" = "1" ] && [ "$hot_backing" != "dedicated" ]; then
    case "$hot_backing" in
      container-root) hot_ok=0; hot_reason="on the container root filesystem" ;;
      kubelet-disk) hot_ok=0; hot_reason="on the node's kubelet disk" ;;
      *) hot_ok=0; hot_reason="on an unknown device" ;;
    esac
  elif [ -n "$hot_avail" ] && [ "$hot_avail" -le "$hot_required_kib" ]; then
    hot_ok=0; hot_reason="insufficient free space (${hot_avail} KiB < ${hot_required_kib} KiB)"
  fi
fi
if [ "$hot_ok" = "1" ]; then
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
if [ "$hot_ok" != "1" ] && [ "${TAU_HOT_REQUIRE_LOCAL:-}" = "1" ]; then
  echo "tau storage error: $TAU_HOT_DIR is $hot_reason; TAU_HOT_REQUIRE_LOCAL=1 forbids falling back to $TAU_DATA_DIR" >&2
  exit 1
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
