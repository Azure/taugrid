#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


# IB Link Flap Detection Script for Node Problem Detector
# Detects InfiniBand link instability through multiple monitoring methods
#
# State format (line-oriented, one snapshot per line):
#   TIMESTAMP DEV1=STATE1 DEV2=STATE2 ...
# Example:
#   1713140000 mlx5_0:1=up mlx5_1:1=up mlx5_2:1=down

set -euo pipefail

# Exit codes
readonly OK=0
readonly NONOK=1
readonly UNKNOWN=2

# Configuration (templated by Helm)
readonly EXPECTED_IB_DEVS="${IB_DEVICES:-}"
readonly FLAP_THRESHOLD="${IB_FLAP_THRESHOLD_SHORT:-2}"
readonly CHECK_WINDOW="${IB_FLAP_CHECK_WINDOW:-3600}"
readonly JOURNAL_FLAP_WINDOW=120

# Storage configuration (host path volume mount)
readonly STATE_FILE="/var/lib/ib-state/ib_flap_state.txt"
readonly TEMP_STATE_FILE="/tmp/ib_flap_state.txt"
readonly LOG_RETENTION_SECONDS=$((CHECK_WINDOW * 2))
readonly MAX_ENTRIES=10

# Logging functions
log_warn() {
    echo "[WARN] $*" >&2
}

log_error() {
    echo "[ERROR] $*" >&2
}

# Shell robustness
safe_command_check() {
    local cmd="$1"
    command -v "$cmd" >/dev/null 2>&1 || which "$cmd" >/dev/null 2>&1 || type "$cmd" >/dev/null 2>&1
}

is_device_monitored() {
    local target_device="$1"
    for ib_dev in $EXPECTED_IB_DEVS; do
        if [[ "$ib_dev" == "$target_device" ]]; then
            return 0
        fi
    done
    return 1
}

# ── State file management ──

get_state_file() {
    if [[ -w "$(dirname "$STATE_FILE")" ]] 2>/dev/null; then
        echo "$STATE_FILE"
    else
        log_warn "Cannot write to host filesystem, using temporary storage"
        echo "$TEMP_STATE_FILE"
    fi
}

load_state() {
    local state_file
    state_file=$(get_state_file)

    if [[ ! -f "$state_file" || ! -r "$state_file" ]]; then
        echo ""
        return 0
    fi

    local content
    content=$(cat "$state_file" 2>/dev/null) || true

    # Detect old JSON format and discard it
    local first_char
    first_char=$(echo "$content" | head -c1)
    if [[ "$first_char" == "[" || "$first_char" == "{" ]]; then
        log_warn "Detected old JSON state file, resetting to empty"
        echo ""
        return 0
    fi

    echo "$content"
}

save_state() {
    local state_data="$1"
    local state_file
    state_file=$(get_state_file)

    local current_time
    current_time=$(date +%s)
    local cutoff=$((current_time - LOG_RETENTION_SECONDS))

    # Filter old entries and keep only last MAX_ENTRIES via awk
    local cleaned
    cleaned=$(echo "$state_data" | awk -v cutoff="$cutoff" -v max="$MAX_ENTRIES" '
        NF > 0 && $1 ~ /^[0-9]+$/ && $1 > cutoff { count++; lines[count] = $0 }
        END {
            start = 1
            if (count > max) start = count - max + 1
            for (k = start; k <= count; k++) print lines[k]
        }
    ')

    # Atomic write via temp + mv
    local temp_file="${state_file}.tmp.$$"
    if echo "$cleaned" > "$temp_file" 2>/dev/null; then
        if mv "$temp_file" "$state_file" 2>/dev/null; then
            return 0
        fi
        rm -f "$temp_file"
    fi
    log_error "Failed to save state file"
    return 1
}

# ── IB state collection ──

get_current_ib_state_ibstat() {
    # Outputs: DEV1=STATE1 DEV2=STATE2 ...
    # States: up, down, unknown

    if ! safe_command_check "ibstat"; then
        log_warn "ibstat command not available"
        # Output all devices as unknown
        for ib_dev in $EXPECTED_IB_DEVS; do
            printf "%s=unknown " "$ib_dev"
        done
        return 0
    fi

    local ibstat_output
    if ! ibstat_output=$(timeout 10s ibstat 2>/dev/null); then
        log_warn "ibstat command failed or timed out"
        for ib_dev in $EXPECTED_IB_DEVS; do
            printf "%s=unknown " "$ib_dev"
        done
        return 0
    fi

    for ib_dev in $EXPECTED_IB_DEVS; do
        local device_name port
        device_name=$(echo "$ib_dev" | sed 's/:.*//')
        port=$(echo "$ib_dev" | sed 's/.*://')

        local state_info
        # Use exact-string comparisons (not substring regex) so dev="mlx5_1"
        # does not also match "mlx5_10"/"mlx5_11" on systems with ≥10 HCAs,
        # and port="1" does not also match "Port 10:"/"Port 11:".
        state_info=$(echo "$ibstat_output" | awk -v dev="'$device_name'" -v port_tok="${port}:" '
            /^CA / {
                in_device = ($2 == dev)
                in_port = 0
                next
            }
            in_device && $1 == "Port" && $2 == port_tok { in_port=1; next }
            in_device && in_port && $1 == "State:" { print $2; in_port=0; in_device=0 }
        ')

        local normalized_state="unknown"
        case "${state_info:-}" in
            "Active") normalized_state="up" ;;
            "Down"|"Init"|"Armed") normalized_state="down" ;;
        esac

        printf "%s=%s " "$ib_dev" "$normalized_state"
    done
}

# Build a snapshot line: "TIMESTAMP DEV1=STATE1 DEV2=STATE2 ..."
build_snapshot() {
    local current_time
    current_time=$(date +%s)
    local device_states
    device_states=$(get_current_ib_state_ibstat)
    echo "${current_time} ${device_states}"
}

# ── Flap analysis ──

# Extract the state of a device from a snapshot line
get_device_state() {
    local line="$1"
    local device="$2"
    echo "$line" | tr ' ' '\n' | awk -F= -v dev="$device" '$1 == dev { print $2; exit }'
}

# Analyze state history for link flaps (ibstat transitions)
# Returns 0 (OK) or 1 (NONOK)
analyze_flaps() {
    local state_data="$1"
    local current_time
    current_time=$(date +%s)
    local time_window=$((current_time - CHECK_WINDOW))
    local issues_found=false
    local flap_reports=()

    for ib_dev in $EXPECTED_IB_DEVS; do
        local transitions=0
        local prev_state=""

        # Read snapshots in order, count state transitions within window
        while IFS= read -r line; do
            [[ -z "$line" ]] && continue
            local ts
            ts=$(echo "$line" | awk '{print $1}')
            [[ ! "$ts" =~ ^[0-9]+$ ]] && continue
            [[ "$ts" -lt "$time_window" ]] && { prev_state=$(get_device_state "$line" "$ib_dev"); continue; }

            local cur_state
            cur_state=$(get_device_state "$line" "$ib_dev")
            cur_state="${cur_state:-unknown}"

            if [[ -n "$prev_state" && "$prev_state" != "$cur_state" ]]; then
                transitions=$((transitions + 1))
            fi
            prev_state="$cur_state"
        done <<< "$state_data"

        # Match original semantics: complete flaps = transitions / 2
        local complete_flaps=$((transitions / 2))
        if [[ $complete_flaps -ge $FLAP_THRESHOLD ]]; then
            flap_reports+=("$ib_dev: $complete_flaps ibstat state flaps in CHECK_WINDOW (threshold: $FLAP_THRESHOLD)")
            issues_found=true
        fi
    done

    # Channel 2: journalctl kernel log analysis
    local journal_issues
    journal_issues=$(analyze_journalctl_flaps "$current_time")
    if [[ -n "$journal_issues" ]]; then
        while IFS= read -r report; do
            [[ -n "$report" ]] && flap_reports+=("$report")
        done <<< "$journal_issues"
        issues_found=true
    fi

    if [[ "$issues_found" == "true" ]]; then
        echo "IB link flaps detected: $(printf '%s; ' "${flap_reports[@]}" | sed 's/; $//')"
        return $NONOK
    else
        echo "IB link stability check passed"
        return $OK
    fi
}

# Analyze journalctl for link flaps
analyze_journalctl_flaps() {
    local current_time="$1"

    if ! safe_command_check "journalctl"; then
        return 0
    fi

    local journal_dir
    journal_dir="$(ls -td /var/log/journal/*/ 2>/dev/null | head -1 | sed 's:/$::')"
    [[ -z "$journal_dir" ]] && return 0

    local since_ts=$((current_time - JOURNAL_FLAP_WINDOW))
    local journal_output
    if ! journal_output=$(journalctl -D "$journal_dir" -k --since "@${since_ts}" --output=short --no-pager 2>/dev/null); then
        return 0
    fi

    local regex='([[:alnum:]_.-]+):[[:space:]]+Port:[[:space:]]+([0-9]+)[[:space:]]+Link[[:space:]]+(DOWN|ACTIVE|INIT)'

    # Track per-device: last_state and transition count using temp files
    # (associative arrays not available in all bash versions on minimal images)
    local tmpdir
    tmpdir=$(mktemp -d)

    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        if [[ $line =~ $regex ]]; then
            local device_name="${BASH_REMATCH[1]}"
            local port="${BASH_REMATCH[2]}"
            local state="${BASH_REMATCH[3]}"

            [[ "$state" == "INIT" ]] && continue

            local normalized_state
            case "$state" in
                "ACTIVE") normalized_state="up" ;;
                "DOWN") normalized_state="down" ;;
                *) continue ;;
            esac

            local device="${device_name}:${port}"
            is_device_monitored "$device" || continue

            local prev_state=""
            [[ -f "$tmpdir/${device}.last" ]] && prev_state=$(cat "$tmpdir/${device}.last")

            if [[ -n "$prev_state" && "$prev_state" != "$normalized_state" ]]; then
                local count=0
                [[ -f "$tmpdir/${device}.count" ]] && count=$(cat "$tmpdir/${device}.count")
                echo $((count + 1)) > "$tmpdir/${device}.count"
            fi

            echo "$normalized_state" > "$tmpdir/${device}.last"
        fi
    done <<< "$journal_output"

    # Collect results
    for ib_dev in $EXPECTED_IB_DEVS; do
        if [[ -f "$tmpdir/${ib_dev}.count" ]]; then
            local flaps
            flaps=$(cat "$tmpdir/${ib_dev}.count")
            if [[ "$flaps" -gt 0 ]] 2>/dev/null; then
                echo "$ib_dev: journalctl detected $flaps link flap(s) within last ${JOURNAL_FLAP_WINDOW}s"
            fi
        fi
    done

    rm -rf "$tmpdir"
}

# ── Main ──

main() {
    # Bail out early if no devices configured
    if [[ -z "$EXPECTED_IB_DEVS" ]]; then
        echo "No IB devices configured, skipping check"
        exit $OK
    fi

    # Load existing state
    local state_data
    state_data=$(load_state)

    # Take new snapshot and append
    local snapshot
    snapshot=$(build_snapshot)
    if [[ -n "$state_data" ]]; then
        state_data="${state_data}
${snapshot}"
    else
        state_data="$snapshot"
    fi

    # Save updated state (handles cleanup and rotation)
    if ! save_state "$state_data"; then
        log_warn "Failed to save state, continuing with analysis"
    fi

    # Analyze for flaps and report
    if analyze_flaps "$state_data"; then
        exit $OK
    else
        exit $NONOK
    fi
}

main "$@"
