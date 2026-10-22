#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""
RL prompt extraction pipeline.

Reads ~60K raw COBOL source files from the PVC, deduplicates, extracts
PROCEDURE DIVISION prompts, compile-tests each with cobc, and writes
an RL-ready JSONL.

Programs that share any VAL_OVERLAP_LINES consecutive normalized lines with a
val.jsonl program are excluded, because 03-benchmarks builds the eval suites
from val.jsonl. The stage fails if val.jsonl is missing or empty, or if any
written prompt still overlaps the validation split.

Input:  /data/datasets/cobol/source-file-index.txt (rebuilt by 01-collect/collect-rl-data.sh)
        /data/datasets/cobol/raw/**/*.{cbl,cob,cpy}
        /data/datasets/cobol/prepared/val.jsonl (from 02-data-prep)
Output: /data/datasets/cobol/prepared/rl-prompts.jsonl
        /data/datasets/cobol/prepared/rl-prep-report.json

Compatible with 06-grpo/train_grpo.py's load_prompts() — each JSONL row has
{"text": "<full program>"} where the program contains both the prompt
prefix (up to PROCEDURE DIVISION header) and the body.

Requires: cobc (GnuCOBOL) installed in the container.
"""

import hashlib
import json
import os
import re
import subprocess
import tempfile
import time
from collections import Counter
from concurrent.futures import ProcessPoolExecutor, as_completed

RAW = os.environ.get("RAW", "/data/datasets/cobol/raw")
DATA_ROOT = os.environ.get("DATA_ROOT", "/data/datasets/cobol")
OUT = os.environ.get("OUT", "/data/datasets/cobol/prepared")
MAX_WORKERS = int(os.environ.get("MAX_WORKERS", "8"))
MIN_BODY_LINES = int(os.environ.get("MIN_BODY_LINES", "5"))
MAX_BODY_LINES = int(os.environ.get("MAX_BODY_LINES", "200"))
MAX_PROMPT_CHARS = int(os.environ.get("MAX_PROMPT_CHARS", "50000"))
VAL_PATH = os.environ.get("VAL", os.path.join(OUT, "val.jsonl"))
VAL_OVERLAP_LINES = int(os.environ.get("VAL_OVERLAP_LINES", "8"))

os.makedirs(OUT, exist_ok=True)

COBOL_EXTS = {".cbl", ".cob", ".cobol", ".cpy", ".CBL", ".COB", ".CPY"}

PROC_DIV_RE = re.compile(
    r"^(.*PROCEDURE\s+DIVISION[^\n]*\.?)$", re.IGNORECASE | re.MULTILINE
)

EXEC_BLOCK_RE = re.compile(
    r"EXEC\s+(SQL|CICS|DLI)\b.*?END-EXEC\s*\.?", re.IGNORECASE | re.DOTALL
)

COPY_STMT_RE = re.compile(r"^\s*COPY\s+\S+.*?\.\s*$", re.IGNORECASE | re.MULTILINE)

WHITESPACE_RE = re.compile(r"\s+")

stats = Counter()


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def discover_files():
    index_path = os.path.join(DATA_ROOT, "source-file-index.txt")
    source_files = []
    if os.path.isfile(index_path):
        log(f"Reading file index from {index_path}")
        with open(index_path) as f:
            for line in f:
                fpath = line.strip()
                if not fpath:
                    continue
                ext = os.path.splitext(fpath)[1].lower()
                if ext in {e.lower() for e in COBOL_EXTS}:
                    source_files.append(fpath)
    else:
        log("No file index found, walking raw directory...")
        for dirpath, _, filenames in os.walk(RAW):
            for fname in filenames:
                if os.path.splitext(fname)[1] in COBOL_EXTS:
                    source_files.append(os.path.join(dirpath, fname))
    log(f"Discovered {len(source_files)} source files")
    return source_files


def exact_dedup(texts_with_paths):
    seen = set()
    unique = []
    for path, text in texts_with_paths:
        h = hashlib.sha256(text.encode("utf-8", errors="replace")).hexdigest()
        if h not in seen:
            seen.add(h)
            unique.append((path, text))
    duped = len(texts_with_paths) - len(unique)
    return unique, duped


def normalized_lines(text):
    return [
        WHITESPACE_RE.sub(" ", ln).strip().upper()
        for ln in text.replace("\r", "").split("\n")
        if ln.strip()
    ]


def line_windows(text, size):
    lines = normalized_lines(text)
    return {
        hashlib.sha256("\n".join(lines[i : i + size]).encode()).hexdigest()
        for i in range(max(0, len(lines) - size + 1))
    }


def load_val_windows(path, size):
    if not os.path.isfile(path):
        raise RuntimeError(f"{path} not found; run 02-data-prep first")
    windows = set()
    count = 0
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            windows |= line_windows(json.loads(line).get("text", ""), size)
            count += 1
    if not count or not windows:
        raise RuntimeError(f"{path} has no validation programs")
    return windows, count


def overlaps_val(text, val_windows, size):
    return not line_windows(text, size).isdisjoint(val_windows)


def exclude_val_overlap(texts_with_paths, val_windows, size):
    kept = [
        (path, text)
        for path, text in texts_with_paths
        if not overlaps_val(text, val_windows, size)
    ]
    return kept, len(texts_with_paths) - len(kept)


def assert_disjoint_from_val(records, val_windows, size):
    leaked = [r["path"] for r in records if overlaps_val(r["text"], val_windows, size)]
    if leaked:
        raise RuntimeError(
            f"{len(leaked)} RL prompts overlap the validation split, e.g. {leaked[0]}"
        )


def extract_prompt(text):
    upper = text.upper()
    if "IDENTIFICATION DIVISION" not in upper and "PROGRAM-ID" not in upper:
        return None, None, 0
    if "PROCEDURE DIVISION" not in upper:
        return None, None, 0
    m = PROC_DIV_RE.search(text)
    if not m:
        return None, None, 0
    prompt = text[: m.end()]
    body = text[m.end() :]
    body_lines = [ln for ln in body.split("\n") if ln.strip()]
    return prompt, body, len(body_lines)


def strip_exec_blocks(text):
    return EXEC_BLOCK_RE.sub("           CONTINUE", text)


def strip_copy_stmts(text):
    return COPY_STMT_RE.sub("", text)


def try_compile_free(cobol_text):
    with tempfile.NamedTemporaryFile(suffix=".cbl", mode="w", delete=False) as f:
        f.write(cobol_text)
        f.flush()
        src = f.name
    try:
        result = subprocess.run(
            [
                "cobc",
                "-fsyntax-only",
                "-free",
                "-frelax-syntax-checks",
                "-frelax-level-hierarchy",
                "-fmissing-statement=ok",
                "-fmissing-period=ok",
                "-w",
                src,
            ],
            capture_output=True,
            timeout=10,
        )
        stderr = result.stderr.decode("utf-8", errors="replace")
        return result.returncode, stderr
    except subprocess.TimeoutExpired:
        return -1, "timeout"
    except Exception as e:
        return -1, str(e)
    finally:
        try:
            os.unlink(src)
        except OSError:
            pass


def try_compile_fixed(cobol_text):
    with tempfile.NamedTemporaryFile(suffix=".cbl", mode="w", delete=False) as f:
        f.write(cobol_text)
        f.flush()
        src = f.name
    try:
        result = subprocess.run(
            [
                "cobc",
                "-fsyntax-only",
                "-fixed",
                "-frelax-syntax-checks",
                "-frelax-level-hierarchy",
                "-fmissing-statement=ok",
                "-fmissing-period=ok",
                "-w",
                src,
            ],
            capture_output=True,
            timeout=10,
        )
        stderr = result.stderr.decode("utf-8", errors="replace")
        return result.returncode, stderr
    except subprocess.TimeoutExpired:
        return -1, "timeout"
    except Exception as e:
        return -1, str(e)
    finally:
        try:
            os.unlink(src)
        except OSError:
            pass


def is_copybook_only_error(stderr):
    if not stderr.strip():
        return False
    for line in stderr.splitlines():
        low = line.lower()
        if "error:" in low:
            if "copy" not in low or "not found" not in low:
                return False
    return True


def compile_check(text):
    rc, stderr = try_compile_free(text)
    if rc == 0:
        return "free", rc, stderr
    if is_copybook_only_error(stderr):
        return "free-copybook-only", 0, stderr
    rc2, stderr2 = try_compile_fixed(text)
    if rc2 == 0:
        return "fixed", rc2, stderr2
    if is_copybook_only_error(stderr2):
        return "fixed-copybook-only", 0, stderr2
    return "failed", rc, stderr


def process_one(path_and_text):
    path, raw_text = path_and_text
    result = {"path": path, "status": None}

    text = raw_text.replace("\r\n", "\n").replace("\r", "\n")
    text = text.rstrip()

    text_stripped = strip_exec_blocks(text)
    text_stripped = strip_copy_stmts(text_stripped)

    prompt, body, body_lines = extract_prompt(text_stripped)
    if prompt is None:
        result["status"] = "no_proc_div"
        return result
    if body_lines < MIN_BODY_LINES:
        result["status"] = "body_too_short"
        return result
    if body_lines > MAX_BODY_LINES:
        result["status"] = "body_too_long"
        return result
    if len(prompt) > MAX_PROMPT_CHARS:
        result["status"] = "prompt_too_long"
        return result

    full = prompt + body
    fmt, rc, stderr = compile_check(full)
    if rc != 0:
        result["status"] = "compile_fail"
        result["compile_fmt"] = fmt
        return result

    result["status"] = "ok"
    result["compile_fmt"] = fmt
    result["body_lines"] = body_lines
    result["text"] = text_stripped
    return result


def main():
    log("=== RL Prompt Extraction Pipeline ===")
    log(f"RAW={RAW}, OUT={OUT}, workers={MAX_WORKERS}")
    log(f"Body lines: {MIN_BODY_LINES}-{MAX_BODY_LINES}")

    subprocess.run(["cobc", "--version"], capture_output=True, check=True)
    log("cobc available")

    val_windows, val_count = load_val_windows(VAL_PATH, VAL_OVERLAP_LINES)
    log(f"Loaded {val_count} validation programs from {VAL_PATH}")

    file_paths = discover_files()

    log("Reading source files...")
    texts = []
    read_errors = 0
    for fpath in file_paths:
        try:
            with open(fpath, "r", errors="replace") as f:
                content = f.read()
            if len(content.strip()) > 50:
                texts.append((fpath, content))
        except Exception:
            read_errors += 1
    log(f"Read {len(texts)} files ({read_errors} errors)")

    texts, duped = exact_dedup(texts)
    log(f"After exact dedup: {len(texts)} unique ({duped} duplicates removed)")

    texts, val_overlap = exclude_val_overlap(texts, val_windows, VAL_OVERLAP_LINES)
    log(
        f"After validation-split exclusion: {len(texts)} remain "
        f"({val_overlap} share {VAL_OVERLAP_LINES}+ lines with val.jsonl)"
    )

    log(f"Processing {len(texts)} files with {MAX_WORKERS} workers...")
    results = []
    done = 0
    ok_count = 0
    with ProcessPoolExecutor(max_workers=MAX_WORKERS) as pool:
        futures = {pool.submit(process_one, item): item[0] for item in texts}
        for future in as_completed(futures):
            try:
                r = future.result()
                results.append(r)
                if r["status"] == "ok":
                    ok_count += 1
            except Exception as e:
                results.append(
                    {"path": futures[future], "status": "error", "error": str(e)}
                )
            done += 1
            if done % 5000 == 0:
                log(
                    f"  ... {done}/{len(texts)} processed, {ok_count} compilable so far"
                )

    for r in results:
        stats[r["status"]] += 1

    log("Processing complete:")
    for status, count in stats.most_common():
        log(f"  {status}: {count}")

    ok_results = [r for r in results if r["status"] == "ok"]
    assert_disjoint_from_val(ok_results, val_windows, VAL_OVERLAP_LINES)
    log(f"Writing {len(ok_results)} compilable programs to rl-prompts.jsonl...")

    out_path = os.path.join(OUT, "rl-prompts.jsonl")
    with open(out_path, "w") as f:
        for r in ok_results:
            f.write(
                json.dumps(
                    {
                        "text": r["text"],
                        "source": r["path"],
                        "compile_fmt": r["compile_fmt"],
                        "body_lines": r["body_lines"],
                    }
                )
                + "\n"
            )

    out_size = os.path.getsize(out_path)
    log(f"Wrote {out_path} ({out_size / 1024 / 1024:.1f} MB)")

    report = {
        "total_files_discovered": len(file_paths),
        "total_read": len(texts) + duped,
        "exact_dupes_removed": duped,
        "after_dedup": len(texts) + val_overlap,
        "val_overlap_removed": val_overlap,
        "val_overlap_lines": VAL_OVERLAP_LINES,
        "after_val_exclusion": len(texts),
        "status_counts": dict(stats.most_common()),
        "compilable_programs": len(ok_results),
        "output_file": out_path,
        "output_size_bytes": out_size,
        "body_lines_range": [MIN_BODY_LINES, MAX_BODY_LINES],
    }

    body_lines = [r["body_lines"] for r in ok_results]
    if body_lines:
        body_lines.sort()
        report["body_lines_stats"] = {
            "min": body_lines[0],
            "median": body_lines[len(body_lines) // 2],
            "p90": body_lines[int(len(body_lines) * 0.9)],
            "max": body_lines[-1],
            "mean": sum(body_lines) / len(body_lines),
        }

    fmt_counts = Counter(r["compile_fmt"] for r in ok_results)
    report["compile_format_counts"] = dict(fmt_counts.most_common())

    report_path = os.path.join(OUT, "rl-prep-report.json")
    with open(report_path, "w") as f:
        json.dump(report, f, indent=2)
    log(f"Report: {report_path}")

    log(f"=== Done: {len(ok_results)} RL-ready prompts ===")


if __name__ == "__main__":
    main()
