#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -uo pipefail

DATA_ROOT="${DATA_ROOT:-/data/datasets/cobol}"
export RAW="${DATA_ROOT}/raw"
LOGS="${DATA_ROOT}/logs"
mkdir -p "${RAW}" "${LOGS}"

LOG_FILE="${LOGS}/collect-rl.log"
FAILED_STEPS=()

log() { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*" | tee -a "${LOG_FILE}"; }
run_step() {
    local name="$1"; shift
    log "--- ${name} ---"
    if "$@"; then
        log "${name}: OK"
    else
        log "${name}: FAILED"
        FAILED_STEPS+=("${name}")
    fi
}
fail_if_incomplete() {
    local records="$1"
    if [ "${#FAILED_STEPS[@]}" -gt 0 ]; then
        log "ERROR: ${#FAILED_STEPS[@]} required step(s) failed:"
        printf '  %s\n' "${FAILED_STEPS[@]}" | tee -a "${LOG_FILE}"
        exit 1
    fi
    if [ "${records}" -eq 0 ]; then
        log "ERROR: collection produced no COBOL source files"
        exit 1
    fi
}

log "=== RL data collection starting ==="
log "HF_TOKEN set: $([ -n "${HF_TOKEN:-}" ] && echo yes || echo NO)"
SOURCE_LICENSES="${SOURCE_LICENSES:-permissive}"
log "Source licenses: ${SOURCE_LICENSES}"

pip install -q huggingface_hub==1.28.0 datasets==5.0.1 pandas==3.0.6 pyarrow==25.0.1 2>&1 | tail -3 \
    || { log "ERROR: pip install failed"; exit 1; }
log "pip install done"

collect_the_stack_v1() {
    local out="${RAW}/the-stack-v1-cobol"
    [ -f "${out}/metadata.json" ] && { log "Stack v1 already collected, skipping"; return 0; }
    python3 - <<'PYEOF'
import json, os
from datasets import load_dataset

token = os.environ["HF_TOKEN"]
out = os.environ["RAW"] + "/the-stack-v1-cobol"
os.makedirs(out, exist_ok=True)
src_dir = os.path.join(out, "source")
os.makedirs(src_dir, exist_ok=True)

print("Loading bigcode/the-stack COBOL subset...", flush=True)
ds = load_dataset("bigcode/the-stack", data_dir="data/cobol", split="train", token=token)
print(f"  Loaded {len(ds)} records", flush=True)

count = 0
for i, row in enumerate(ds):
    content = row.get("content", "")
    if not content.strip():
        continue
    path = row.get("max_stars_repo_path", row.get("path", f"file_{i}.cbl"))
    ext = os.path.splitext(path)[1].lower()
    if ext not in (".cbl", ".cob", ".cpy"):
        ext = ".cbl"
    fname = f"{i:06d}_{os.path.basename(path)}"
    if not fname.lower().endswith((".cbl", ".cob", ".cpy")):
        fname += ext
    with open(os.path.join(src_dir, fname), "w") as f:
        f.write(content)
    count += 1

with open(os.path.join(out, "metadata.json"), "w") as f:
    json.dump({"source": "bigcode/the-stack", "subset": "cobol", "count": count}, f, indent=2)
print(f"  Saved {count} source files to {src_dir}", flush=True)
PYEOF
}
if [ -n "${HF_TOKEN:-}" ]; then
    run_step "The Stack v1 COBOL (~4K files)" collect_the_stack_v1
else
    log "HF_TOKEN unset: skipping gated dataset the_stack_v1"
fi

collect_the_stack_v2_dedup() {
    local out="${RAW}/the-stack-v2-dedup-cobol"
    [ -f "${out}/metadata.json" ] && { log "Stack v2-dedup already collected, skipping"; return 0; }
    python3 - <<'PYEOF'
import json, os
from datasets import load_dataset

token = os.environ["HF_TOKEN"]
out = os.environ["RAW"] + "/the-stack-v2-dedup-cobol"
os.makedirs(out, exist_ok=True)
src_dir = os.path.join(out, "source")
os.makedirs(src_dir, exist_ok=True)

print("Loading bigcode/the-stack-v2-dedup COBOL...", flush=True)
try:
    ds = load_dataset("bigcode/the-stack-v2-dedup", "COBOL", split="train",
                       token=token)
except Exception as e1:
    print(f"  Config 'COBOL' failed ({e1}), trying data_dir...", flush=True)
    try:
        ds = load_dataset("bigcode/the-stack-v2-dedup", data_dir="data/COBOL",
                           split="train", token=token)
    except Exception as e2:
        print(f"  data_dir 'data/COBOL' failed ({e2}), trying lowercase...", flush=True)
        ds = load_dataset("bigcode/the-stack-v2-dedup", data_dir="data/cobol",
                           split="train", token=token)

print(f"  Loaded {len(ds)} records", flush=True)
print(f"  Columns: {ds.column_names}", flush=True)

has_content = "content" in ds.column_names
count = 0

if has_content:
    for i, row in enumerate(ds):
        content = row.get("content", "")
        if not content or not content.strip():
            continue
        fname = f"{i:06d}.cbl"
        with open(os.path.join(src_dir, fname), "w") as f:
            f.write(content)
        count += 1
    print(f"  Extracted {count} source files", flush=True)
else:
    jsonl_path = os.path.join(out, "the-stack-v2-dedup-cobol.jsonl")
    with open(jsonl_path, "w") as f:
        for row in ds:
            record = {k: v for k, v in row.items() if isinstance(v, (str, int, float, bool))}
            f.write(json.dumps(record) + "\n")
            count += 1
    print(f"  No 'content' column — saved {count} metadata records (SWHIDs only)", flush=True)

with open(os.path.join(out, "metadata.json"), "w") as f:
    json.dump({"source": "bigcode/the-stack-v2-dedup", "subset": "COBOL",
                "count": count, "has_content": has_content}, f, indent=2)
PYEOF
}
if [ -n "${HF_TOKEN:-}" ]; then
    run_step "The Stack v2-dedup COBOL (~36K files)" collect_the_stack_v2_dedup
else
    log "HF_TOKEN unset: skipping gated dataset the_stack_v2_dedup"
fi

collect_pretraining_corpus() {
    local out="${RAW}/cobol-pretraining-corpus"
    [ -f "${out}/metadata.json" ] && { log "Pretraining corpus already collected, skipping"; return 0; }
    python3 - <<'PYEOF'
import json, os
from datasets import load_dataset

token = os.environ["HF_TOKEN"]
out = os.environ["RAW"] + "/cobol-pretraining-corpus"
os.makedirs(out, exist_ok=True)
src_dir = os.path.join(out, "source")
os.makedirs(src_dir, exist_ok=True)

print("Loading 4sp1d3r2/cobol-pretraining-corpus...", flush=True)
ds = load_dataset("4sp1d3r2/cobol-pretraining-corpus", split="train", token=token)
print(f"  Loaded {len(ds)} records", flush=True)
print(f"  Columns: {ds.column_names}", flush=True)

code_count = 0
web_count = 0
for i, row in enumerate(ds):
    text = row.get("text", "")
    source = row.get("source", "")
    if not text or not text.strip():
        continue
    is_code = any(kw in text[:500].upper() for kw in
                  ["IDENTIFICATION DIVISION", "PROCEDURE DIVISION", "DATA DIVISION",
                   "PROGRAM-ID", "WORKING-STORAGE", "01 ", "PERFORM ", "MOVE "])
    if is_code:
        fname = f"{i:06d}.cbl"
        with open(os.path.join(src_dir, fname), "w") as f:
            f.write(text)
        code_count += 1
    else:
        web_count += 1

print(f"  Extracted {code_count} code files, skipped {web_count} web/educational text", flush=True)
with open(os.path.join(out, "metadata.json"), "w") as f:
    json.dump({"source": "4sp1d3r2/cobol-pretraining-corpus",
                "total": len(ds), "code_files": code_count, "web_skipped": web_count}, f, indent=2)
PYEOF
}
if [ -n "${HF_TOKEN:-}" ]; then
    run_step "cobol-pretraining-corpus (~217K rows, extracting code only)" collect_pretraining_corpus
else
    log "HF_TOKEN unset: skipping gated dataset pretraining_corpus"
fi

collect_zorse() {
    local out="${RAW}/zorse"
    [ -f "${out}/metadata.json" ] && { log "zorse already collected, skipping"; return 0; }
    python3 - <<'PYEOF'
import json, os
from datasets import load_dataset

out = os.environ["RAW"] + "/zorse"
os.makedirs(out, exist_ok=True)
src_dir = os.path.join(out, "source")
os.makedirs(src_dir, exist_ok=True)

print("Loading zorse/zorse...", flush=True)
try:
    ds = load_dataset("zorse/zorse", split="train")
except Exception:
    try:
        ds_dict = load_dataset("zorse/zorse")
        first_split = list(ds_dict.keys())[0]
        ds = ds_dict[first_split]
    except Exception as e:
        print(f"  Failed to load zorse/zorse: {e}", flush=True)
        raise

print(f"  Loaded {len(ds)} records", flush=True)
print(f"  Columns: {ds.column_names}", flush=True)

content_col = None
for col in ["content", "text", "code", "source_code", "program"]:
    if col in ds.column_names:
        content_col = col
        break

count = 0
if content_col:
    for i, row in enumerate(ds):
        text = row.get(content_col, "")
        if not text or not text.strip():
            continue
        fname = f"{i:06d}.cbl"
        with open(os.path.join(src_dir, fname), "w") as f:
            f.write(text)
        count += 1
    print(f"  Extracted {count} source files (column: {content_col})", flush=True)
else:
    jsonl_path = os.path.join(out, "zorse.jsonl")
    with open(jsonl_path, "w") as f:
        for row in ds:
            record = {k: v for k, v in row.items() if isinstance(v, (str, int, float, bool))}
            f.write(json.dumps(record) + "\n")
            count += 1
    print(f"  No content column found, saved {count} records as JSONL", flush=True)
    print(f"  Available columns: {ds.column_names}", flush=True)

with open(os.path.join(out, "metadata.json"), "w") as f:
    json.dump({"source": "zorse/zorse", "count": count,
                "content_column": content_col, "columns": ds.column_names}, f, indent=2)
PYEOF
}
if [ "${SOURCE_LICENSES}" = "all" ]; then
    run_step "zorse/zorse (~40K COBOL rows)" collect_zorse
else
    log "skipping zorse/zorse: no stated license (set SOURCE_LICENSES=all to include it)"
fi

log "--- Large GitHub parser repos ---"
GITHUB="${RAW}/github"
mkdir -p "${GITHUB}"

clone_repo() {
    local url="$1" dir="$2"
    if [ -d "${GITHUB}/${dir}" ] && [ "$(find "${GITHUB}/${dir}" -maxdepth 1 -type f 2>/dev/null | head -1)" ]; then
        log "  ${dir} already exists, skipping"
        return 0
    fi
    log "  cloning ${dir}..."
    if ! git clone --depth 1 --single-branch "$url" "${GITHUB}/${dir}" 2>&1 | tail -1; then
        log "  FAILED to clone ${dir}"
        FAILED_STEPS+=("clone ${dir}")
    fi
}

clone_repo "https://github.com/uwol/proleap-cobol-parser.git"            "proleap"
clone_repo "https://github.com/avishek-sen-gupta/cobol-rekt.git"         "cobol-rekt"
clone_repo "https://github.com/mrm413/Ironclad-COBOL-to-Rust.git"        "ironclad"
clone_repo "https://github.com/navikt/DSF.git"                           "dsf-norway"
clone_repo "https://github.com/IBM/Bank-of-Z.git"                        "bank-of-z"
clone_repo "https://github.com/icitry/FPS.cob.git"                       "fps-cobol"

if [ "${SOURCE_LICENSES}" = "all" ]; then
    clone_repo "https://github.com/eclipse-che4z/che-che4z-lsp-for-cobol.git" "che4z-lsp"
    clone_repo "https://github.com/neopragma/cobol-unit-test.git"            "cobol-unit-test"
    clone_repo "https://github.com/mridoni/gixsql.git"                       "gixsql"
    clone_repo "https://github.com/JohnDovey/GNUCobol-Samples.git"           "gnucobol-samples"
else
    log "  skipping copyleft repos (set SOURCE_LICENSES=all to include them)"
fi

NEW_GH=$(find "${GITHUB}/che4z-lsp" "${GITHUB}/proleap" "${GITHUB}/cobol-rekt" "${GITHUB}/ironclad" "${GITHUB}/cobol-unit-test" "${GITHUB}/dsf-norway" "${GITHUB}/bank-of-z" "${GITHUB}/gixsql" "${GITHUB}/gnucobol-samples" "${GITHUB}/fps-cobol" -type f \( -iname '*.cbl' -o -iname '*.cob' -o -iname '*.cobol' -o -iname '*.cpy' -o -iname '*.CBL' -o -iname '*.COB' \) 2>/dev/null | wc -l)
log "New GitHub parser repos: ${NEW_GH} COBOL files"

collect_nist_from_oss4j() {
    local nist_dir="${RAW}/nist-extracted"
    [ -d "${nist_dir}" ] && [ "$(find "${nist_dir}" -name '*.CBL' -o -name '*.cbl' 2>/dev/null | head -1)" ] && {
        log "NIST already extracted, skipping"
        return 0
    }
    mkdir -p "${nist_dir}"

    local oss4j="${GITHUB}/opensourcecobol4j"
    if [ ! -d "${oss4j}" ]; then
        log "opensourcecobol4j not found, cloning..."
        git clone --depth 1 --single-branch "https://github.com/opensourcecobol/opensourcecobol4j.git" "${oss4j}" 2>&1 | tail -1
    fi

    local nist_src
    nist_src=$(find "${oss4j}" -type d -name "CCVS85" -o -name "nist" -o -name "cobol85" 2>/dev/null | head -1)
    if [ -z "${nist_src}" ]; then
        nist_src=$(find "${oss4j}" -type f -iname '*.CBL' -path '*test*' 2>/dev/null | head -1)
        nist_src=$(dirname "${nist_src:-/nonexistent}")
    fi

    if [ -d "${nist_src}" ]; then
        find "${nist_src}" -type f \( -iname '*.cbl' -o -iname '*.CBL' -o -iname '*.cob' -o -iname '*.cpy' \) -exec cp {} "${nist_dir}/" \;
        local count
        count=$(find "${nist_dir}" -type f 2>/dev/null | wc -l)
        log "NIST extracted: ${count} files from ${nist_src}"
    else
        log "NIST: could not find test directory in opensourcecobol4j"
        return 1
    fi
}
if [ "${SOURCE_LICENSES}" = "all" ]; then
    run_step "NIST test suite (from opensourcecobol4j)" collect_nist_from_oss4j
else
    log "skipping NIST extraction from opensourcecobol4j (set SOURCE_LICENSES=all to include it)"
fi

log "=== RL data collection complete ==="
log "New data disk usage:"
for d in the-stack-v1-cobol the-stack-v2-dedup-cobol cobol-pretraining-corpus zorse; do
    du -sh "${RAW}/${d}" 2>/dev/null | tee -a "${LOGS}/collect-rl.log" || true
done
for d in che4z-lsp proleap cobol-rekt ironclad cobol-unit-test dsf-norway bank-of-z gixsql gnucobol-samples fps-cobol nist-extracted; do
    du -sh "${GITHUB}/${d}" 2>/dev/null || du -sh "${RAW}/${d}" 2>/dev/null || true
done | tee -a "${LOGS}/collect-rl.log"

TOTAL_NEW=$(find "${RAW}/the-stack-v1-cobol" "${RAW}/the-stack-v2-dedup-cobol" "${RAW}/cobol-pretraining-corpus" "${RAW}/zorse" "${GITHUB}/che4z-lsp" "${GITHUB}/proleap" "${GITHUB}/cobol-rekt" "${GITHUB}/ironclad" "${GITHUB}/cobol-unit-test" "${GITHUB}/dsf-norway" "${GITHUB}/bank-of-z" "${GITHUB}/gixsql" "${GITHUB}/gnucobol-samples" "${GITHUB}/fps-cobol" "${RAW}/nist-extracted" -type f \( -iname '*.cbl' -o -iname '*.cob' -o -iname '*.cobol' -o -iname '*.cpy' \) 2>/dev/null | wc -l)
log "Total NEW COBOL source files: ${TOTAL_NEW}"

TOTAL_ALL=$(find "${RAW}" -type f \( -iname '*.cbl' -o -iname '*.cob' -o -iname '*.cobol' -o -iname '*.cpy' \) 2>/dev/null | wc -l)
log "Total ALL COBOL source files (old + new): ${TOTAL_ALL}"
fail_if_incomplete "${TOTAL_NEW}"

log "Rebuilding file index..."
find "${RAW}" -type f \( -iname '*.cbl' -o -iname '*.cob' -o -iname '*.cobol' -o -iname '*.cpy' \) > "${DATA_ROOT}/source-file-index.txt"
find "${RAW}" -type f -name '*.jsonl' >> "${DATA_ROOT}/source-file-index.txt"
INDEX_COUNT=$(wc -l < "${DATA_ROOT}/source-file-index.txt")
log "File index rebuilt: ${INDEX_COUNT} entries"
log "=== Done ==="
