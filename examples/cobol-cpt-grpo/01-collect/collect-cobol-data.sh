#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -uo pipefail

DATA_ROOT="${DATA_ROOT:-/data/datasets/cobol}"
export RAW="${DATA_ROOT}/raw"
PROCESSED="${DATA_ROOT}/processed"
LOGS="${DATA_ROOT}/logs"
mkdir -p "${RAW}" "${PROCESSED}" "${LOGS}"

LOG_FILE="${LOGS}/collect.log"
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

log "=== COBOL data collection starting ==="
log "Target: ${DATA_ROOT}"
SOURCE_LICENSES="${SOURCE_LICENSES:-permissive}"
log "Source licenses: ${SOURCE_LICENSES}"

pip install -q huggingface_hub==1.28.0 datasets==5.0.1 pandas==3.0.6 pyarrow==25.0.1 2>&1 | tail -1 \
    || { log "ERROR: pip install failed"; exit 1; }
log "pip install done"

collect_troy_cobol() {
    python3 - <<'PYEOF'
import os, json
from datasets import load_dataset
out = os.environ.get("RAW", "/data/datasets/cobol/raw") + "/troy-cobol"
os.makedirs(out, exist_ok=True)
ds = load_dataset("TroyDoesAI/Cobol", split="train")
for i, row in enumerate(ds):
    path = row.get("path", f"file_{i}.cbl")
    fname = f"{i:06d}_{os.path.basename(path)}"
    with open(os.path.join(out, fname), "w") as f:
        f.write(row["content"])
print(f"Saved {len(ds)} files to {out}")
with open(os.path.join(out, "metadata.json"), "w") as f:
    json.dump({"source": "TroyDoesAI/Cobol", "count": len(ds)}, f)
PYEOF
}
run_step "TroyDoesAI/Cobol (2,980 files)" collect_troy_cobol

collect_commitpack() {
    python3 - <<'PYEOF'
import os, json, shutil
from huggingface_hub import hf_hub_download
out = os.environ.get("RAW", "/data/datasets/cobol/raw") + "/commitpack"
os.makedirs(out, exist_ok=True)
local = hf_hub_download("bigcode/commitpack", "data/cobol/cobol-0001.jsonl", repo_type="dataset")
shutil.copy(local, os.path.join(out, "commitpack-cobol.jsonl"))
count = sum(1 for _ in open(os.path.join(out, "commitpack-cobol.jsonl")))
print(f"Saved {count} commit records to {out}")
PYEOF
}
run_step "bigcode/commitpack COBOL (24,953 commits)" collect_commitpack

collect_mainframebench() {
    python3 - <<'PYEOF'
import os, json
from datasets import load_dataset
out = os.environ.get("RAW", "/data/datasets/cobol/raw") + "/mainframebench"
os.makedirs(out, exist_ok=True)
failed = []
for split_name in ["question_answering", "multiple_choice_question", "COBOL_code_summarization"]:
    try:
        ds = load_dataset("Fsoft-AIC/MainframeBench", split_name, split="train")
        with open(os.path.join(out, f"{split_name}.jsonl"), "w") as f:
            for row in ds:
                f.write(json.dumps(dict(row)) + "\n")
        print(f"  {split_name}: {len(ds)} samples")
    except Exception as e:
        print(f"  {split_name}: failed ({e})")
        failed.append(split_name)
if failed:
    raise SystemExit(f"MainframeBench splits failed: {', '.join(failed)}")
PYEOF
}
run_step "MainframeBench (7,052 samples)" collect_mainframebench

collect_eval_benchmarks() {
    python3 - <<'PYEOF'
import os, json
from datasets import load_dataset
raw = os.environ.get("RAW", "/data/datasets/cobol/raw")
failed = []
for name, hf_id in [
    ("coboleval", "msplits/human-eval-cobol"),
    ("cobolcodebench", "harshini-kumar/CobolCodeBench"),
]:
    out = os.path.join(raw, name)
    os.makedirs(out, exist_ok=True)
    try:
        splits = ["default", "train"]
        ds = None
        for s in splits:
            try:
                ds = load_dataset(hf_id, split=s)
                break
            except Exception:
                continue
        if ds is None:
            ds = load_dataset(hf_id)
            first_split = list(ds.keys())[0]
            ds = ds[first_split]
        with open(os.path.join(out, f"{name}.jsonl"), "w") as f:
            for row in ds:
                f.write(json.dumps(dict(row)) + "\n")
        print(f"{name}: {len(ds)} samples")
    except Exception as e:
        print(f"{name}: error - {e}")
        failed.append(name)
if failed:
    raise SystemExit(f"Evaluation datasets failed: {', '.join(failed)}")
PYEOF
}
run_step "Evaluation benchmarks" collect_eval_benchmarks

collect_xcobol() {
    mkdir -p "${RAW}/x-cobol/extracted"
    curl -fsSL "https://zenodo.org/api/records/14269462/files/COBOL_Files.zip/content" -o "${RAW}/x-cobol/extracted/COBOL_Files.zip" || return 1
    echo "9b97b2b77f025a11895730580965bc79ab3d715f9922ab79c0a67e30068c6e6d  ${RAW}/x-cobol/extracted/COBOL_Files.zip" | sha256sum -c - || return 1
    (cd "${RAW}/x-cobol/extracted" && unzip -qo COBOL_Files.zip -d COBOL_Files) || return 1
    XCOUNT=$(find ${RAW}/x-cobol/extracted -type f \( -iname '*.cbl' -o -iname '*.cob' -o -iname '*.cobol' -o -iname '*.cpy' \) 2>/dev/null | wc -l)
    log "X-COBOL: ${XCOUNT} COBOL files"
    if [ "${XCOUNT}" -eq 0 ]; then
        log "X-COBOL: listing top-level contents for debugging:"
        find "${RAW}/x-cobol/extracted" -maxdepth 3 -type f 2>/dev/null | head -30 | tee -a "${LOGS}/collect.log"
        return 1
    fi
}
run_step "X-COBOL v2 from Zenodo" collect_xcobol

log "--- GitHub repos ---"
GITHUB="${RAW}/github"
mkdir -p "${GITHUB}"

clone_repo() {
    local url="$1" dir="$2"
    if [ ! -d "${GITHUB}/${dir}/.git" ]; then
        log "  cloning ${dir}..."
        if ! git clone --depth 1 --single-branch "$url" "${GITHUB}/${dir}" 2>&1 | tail -1; then
            log "  FAILED to clone ${dir}"
            FAILED_STEPS+=("clone ${dir}")
        fi
    else
        log "  ${dir} already exists, skipping"
    fi
}

clone_repo "https://github.com/aws-samples/aws-mainframe-modernization-carddemo.git" "carddemo"
clone_repo "https://github.com/meyfa/CobolCraft.git"                    "cobolcraft"
clone_repo "https://github.com/openmainframeproject/cobol-programming-course.git" "omp-course"
clone_repo "https://github.com/DillonDepeel/Cobol-Programming-Collection.git" "cobol-collection"
clone_repo "https://github.com/shamrice/COBOL-Examples.git"              "cobol-examples"
clone_repo "https://github.com/krisds/koopa.git"                        "koopa"
clone_repo "https://github.com/bushidocodes/pc-pricer.git"              "cms-pricer"
clone_repo "https://github.com/exercism/cobol.git"                      "exercism-cobol"

if [ "${SOURCE_LICENSES}" = "all" ]; then
    clone_repo "https://github.com/OCamlPro/gnucobol-contrib.git"          "gnucobol-contrib"
    clone_repo "https://github.com/opensourcecobol/opensourcecobol4j.git"   "opensourcecobol4j"
    clone_repo "https://github.com/Martinfx/Cobol.git"                      "martinfx-cobol"
    clone_repo "https://github.com/etalab/taxe-fonciere.git"                "taxe-fonciere"
    clone_repo "https://github.com/josuerochadev/poei-cobol.git"            "poei-cobol"
    clone_repo "https://github.com/cchipman21804/EnterpriseCOBOLv6.3.git"   "enterprise-cobol"
    clone_repo "https://github.com/Apress/beg-cobol-for-programmers.git"    "apress-cobol"
    clone_repo "https://github.com/andrea-git/GESLUX.git"                   "geslux"
    clone_repo "https://github.com/z390development/z390.git"                "z390"
    clone_repo "https://github.com/writ3it/cobol-examples.git"              "writ3it-examples"
    clone_repo "https://github.com/fgregg/tax_extension.git"                "cook-county-tax"
    clone_repo "https://github.com/sentientsergio/COBOL-Legacy-Benchmark-Suite.git" "legacy-benchmark"
else
    log "  skipping copyleft and unlicensed repos (set SOURCE_LICENSES=all to include them)"
fi

TOTAL_GH=$(find "${GITHUB}" -type f \( -iname '*.cbl' -o -iname '*.cob' -o -iname '*.cobol' -o -iname '*.cpy' -o -iname '*.CBL' -o -iname '*.COB' \) 2>/dev/null | wc -l)
log "GitHub repos: ${TOTAL_GH} COBOL files total"

collect_nist() {
    mkdir -p "${RAW}/nist"
    if [ ! -f "${RAW}/nist/newcob.val" ]; then
        curl -fsSL "https://downloads.sourceforge.net/project/gnucobol/nist/newcob.val.tar.gz" -o "${RAW}/nist/newcob.val.tar.gz" || return 1
        echo "e4513f26a9b38911f7bf882fe3d3339a80b45cabc2caf85eb3055b0f5ce87ee0  ${RAW}/nist/newcob.val.tar.gz" | sha256sum -c - || return 1
        tar xzf "${RAW}/nist/newcob.val.tar.gz" -C "${RAW}/nist" || return 1
    fi
    [ -s "${RAW}/nist/newcob.val" ] || return 1
    log "NIST: newcob.val $(wc -l < "${RAW}/nist/newcob.val") lines"
}
run_step "NIST COBOL85 test suite" collect_nist

log "=== Collection complete ==="
log "Disk usage:"
du -sh "${RAW}"/* 2>/dev/null | tee -a "${LOGS}/collect.log"
log "Total raw:"
du -sh "${RAW}" | tee -a "${LOGS}/collect.log"

TOTAL_COBOL=$(find "${RAW}" -type f \( -iname '*.cbl' -o -iname '*.cob' -o -iname '*.cobol' -o -iname '*.cpy' \) | wc -l)
TOTAL_JSONL=$(find "${RAW}" -type f -name '*.jsonl' | wc -l)
log "Total COBOL source files: ${TOTAL_COBOL}"
log "Total JSONL data files: ${TOTAL_JSONL}"
fail_if_incomplete "${TOTAL_COBOL}"

cat > "${DATA_ROOT}/manifest.json" <<MANIFEST
{
  "collected_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "total_cobol_files": ${TOTAL_COBOL},
  "total_jsonl_files": ${TOTAL_JSONL},
  "sources": {
    "troy_cobol": "${RAW}/troy-cobol",
    "commitpack": "${RAW}/commitpack",
    "mainframebench": "${RAW}/mainframebench",
    "coboleval": "${RAW}/coboleval",
    "cobolcodebench": "${RAW}/cobolcodebench",
    "x_cobol_v2": "${RAW}/x-cobol",
    "github_repos": "${RAW}/github",
    "nist_ccvs85": "${RAW}/nist"
  },
  "source_licenses": "${SOURCE_LICENSES}"
}
MANIFEST
log "Manifest written to ${DATA_ROOT}/manifest.json"

log "Building file index..."
find "${RAW}" -type f \( -iname '*.cbl' -o -iname '*.cob' -o -iname '*.cobol' -o -iname '*.cpy' \) > "${DATA_ROOT}/source-file-index.txt"
find "${RAW}" -type f -name '*.jsonl' >> "${DATA_ROOT}/source-file-index.txt"
INDEX_COUNT=$(wc -l < "${DATA_ROOT}/source-file-index.txt")
log "File index written: ${INDEX_COUNT} entries"
log "=== Done ==="
