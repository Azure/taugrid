#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""
COBOL training data preparation pipeline.

Reads raw collected data from /data/datasets/cobol/raw/,
applies dedup, quality filtering, formatting, and train/val split.
Writes training-ready JSONL to /data/datasets/cobol/prepared/.

Stages:
  1. Ingest — load all COBOL source files and JSONL records into a unified list
  2. Normalize — strip trailing whitespace, normalize line endings, detect format
  3. Dedup — SHA-256 exact dedup, then MinHash near-dedup (Jaccard > 0.7)
  4. Quality filter — remove stubs (<20 non-empty lines), binaries, generated listings
  5. Format — produce training JSONL in completion format with metadata
  6. Split — 95/5 train/val by file, write shards

Uses Ray for parallel processing when available, falls back to multiprocessing.
"""

import hashlib
import json
import os
import re
import random
from collections import defaultdict
from pathlib import Path

RAW = os.environ.get("RAW", "/data/datasets/cobol/raw")
OUT = os.environ.get("OUT", "/data/datasets/cobol/prepared")
REPORT = os.environ.get("REPORT", "/data/datasets/cobol/prepared/prep-report.json")
SEED = 42

os.makedirs(OUT, exist_ok=True)
random.seed(SEED)

COBOL_EXTS = {".cbl", ".cob", ".cobol", ".cpy", ".CBL", ".COB", ".CPY"}


def discover_files_from_index(raw_dir, data_root):
    """BlobFuse with cancel-list-on-mount-seconds can't list large dirs.
    Use the tokenizer per-file-stats.jsonl and known JSONL paths from
    the manifest as an authoritative file index instead of os.walk."""
    per_file_stats = os.path.join(
        data_root, "tokenizer-analysis", "per-file-stats.jsonl"
    )

    source_paths = []
    jsonl_paths = []

    file_index = os.path.join(data_root, "source-file-index.txt")

    if os.path.isfile(per_file_stats):
        print(f"  Reading file index from {per_file_stats}")
        with open(per_file_stats, "r") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    rec = json.loads(line)
                    rel = rec.get("file", "")
                    if rel:
                        full = os.path.join(raw_dir, rel)
                        source_paths.append(full)
                except (json.JSONDecodeError, KeyError):
                    pass
        print(f"  Found {len(source_paths)} source file paths from tokenizer index")
    elif os.path.isfile(file_index):
        print(f"  Reading file index from {file_index}")
        with open(file_index, "r") as f:
            for line in f:
                fpath = line.strip()
                if not fpath:
                    continue
                if any(fpath.lower().endswith(ext) for ext in COBOL_EXTS):
                    source_paths.append(fpath)
                elif fpath.endswith(".jsonl"):
                    jsonl_paths.append(fpath)
        print(
            f"  Found {len(source_paths)} source files and {len(jsonl_paths)} JSONL files from file index"
        )
    else:
        print("  WARNING: no file index found, walking known subdirectories")
        known_subdirs = [
            "troy-cobol",
            "github",
            "x-cobol",
            "nist",
            "the-stack-v1-cobol",
        ]
        for subdir in known_subdirs:
            subpath = os.path.join(raw_dir, subdir)
            if not os.path.isdir(subpath):
                continue
            count = 0
            for dirpath, _, filenames in os.walk(subpath):
                for fname in filenames:
                    if any(fname.endswith(ext) for ext in COBOL_EXTS):
                        source_paths.append(os.path.join(dirpath, fname))
                        count += 1
            print(f"    {subdir}: {count} source files")

    indexed_set = set(source_paths)
    extra_source_dirs = [
        os.path.join(raw_dir, "the-stack-v1-cobol", "source"),
    ]
    extra_count = 0
    for src_dir in extra_source_dirs:
        if not os.path.isdir(src_dir):
            continue
        for fname in os.listdir(src_dir):
            fpath = os.path.join(src_dir, fname)
            if fpath not in indexed_set and any(
                fname.lower().endswith(ext) for ext in COBOL_EXTS
            ):
                source_paths.append(fpath)
                extra_count += 1
    if extra_count:
        print(f"  Found {extra_count} additional source files from new datasets")

    known_jsonl = [
        os.path.join(raw_dir, "commitpack", "commitpack-cobol.jsonl"),
        os.path.join(raw_dir, "mainframebench", "question_answering.jsonl"),
        os.path.join(raw_dir, "mainframebench", "multiple_choice_question.jsonl"),
        os.path.join(raw_dir, "mainframebench", "COBOL_code_summarization.jsonl"),
        os.path.join(raw_dir, "coboleval", "coboleval.jsonl"),
        os.path.join(raw_dir, "cobolcodebench", "cobolcodebench.jsonl"),
        os.path.join(raw_dir, "the-stack-v1-cobol", "the-stack-v1-cobol.jsonl"),
        os.path.join(
            raw_dir, "cobol-pretraining-corpus", "cobol-pretraining-corpus.jsonl"
        ),
    ]
    for jpath in known_jsonl:
        if os.path.isfile(jpath):
            jsonl_paths.append(jpath)
    print(f"  Found {len(jsonl_paths)} JSONL files from known paths")

    return source_paths, jsonl_paths


def ingest_source_files_from_paths(raw_dir, file_paths):
    """Load COBOL source files from a known list of paths."""
    files = []
    errors = 0
    for fpath in file_paths:
        rel = os.path.relpath(fpath, raw_dir)
        try:
            with open(fpath, "r", errors="replace") as f:
                content = f.read()
            files.append({"source": rel, "content": content, "type": "source"})
        except Exception:
            errors += 1
    if errors:
        print(f"  WARNING: {errors} files could not be read")
    return files


def ingest_jsonl_from_paths(raw_dir, jsonl_paths):
    """Load structured records from known JSONL file paths."""
    records = []
    for jpath in jsonl_paths:
        rel = os.path.relpath(jpath, raw_dir)
        source_name = Path(jpath).stem
        try:
            with open(jpath, "r") as f:
                for i, line in enumerate(f):
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        rec = json.loads(line)
                    except json.JSONDecodeError:
                        continue

                    content = extract_content_from_record(rec, source_name)
                    if content and len(content.strip()) > 50:
                        records.append(
                            {
                                "source": f"{rel}:{i}",
                                "content": content,
                                "type": "jsonl",
                                "dataset": source_name,
                            }
                        )
        except Exception:
            pass
    return records


def extract_content_from_record(rec, source_name):
    """Extract COBOL-relevant text from a JSONL record."""
    if "commitpack" in source_name:
        old = rec.get("old_contents", "")
        new = rec.get("new_contents", "")
        if new and looks_like_cobol(new):
            return new
        if old and looks_like_cobol(old):
            return old
        return None

    if "mainframebench" in source_name.lower():
        q = rec.get("question", rec.get("input", ""))
        a = rec.get("answer", rec.get("output", ""))
        if a and looks_like_cobol(a):
            return a
        if q and looks_like_cobol(q):
            return q
        return f"{q}\n{a}" if q and a else None

    if "coboleval" in source_name.lower() or "cobolcodebench" in source_name.lower():
        prompt = rec.get("prompt", rec.get("input", ""))
        solution = rec.get(
            "canonical_solution", rec.get("solution", rec.get("output", ""))
        )
        if solution and looks_like_cobol(solution):
            return solution
        return prompt if prompt else None

    if "the-stack" in source_name.lower():
        content = rec.get("content", "")
        if content and len(content.strip()) > 50:
            return content
        return None

    if "pretraining-corpus" in source_name.lower():
        for key in ["text", "content", "code"]:
            val = rec.get(key, "")
            if val and isinstance(val, str) and len(val.strip()) > 50:
                return val
        return None

    for key in ["content", "code", "text", "source", "output", "new_contents"]:
        val = rec.get(key, "")
        if val and isinstance(val, str) and len(val) > 50:
            return val
    return None


def looks_like_cobol(text):
    """Heuristic: does this text look like COBOL code?"""
    upper = text.upper()
    indicators = [
        "IDENTIFICATION DIVISION",
        "PROCEDURE DIVISION",
        "DATA DIVISION",
        "WORKING-STORAGE",
        "PERFORM ",
        "MOVE ",
        "PIC ",
        "PICTURE ",
        "01 ",
        "05 ",
        "DISPLAY ",
        "STOP RUN",
        "GOBACK",
    ]
    return sum(1 for ind in indicators if ind in upper) >= 2


def normalize(doc):
    """Normalize whitespace and line endings."""
    content = doc["content"]
    content = content.replace("\r\n", "\n").replace("\r", "\n")
    lines = content.split("\n")
    lines = [line.rstrip() for line in lines]
    while lines and not lines[-1].strip():
        lines.pop()
    doc["content"] = "\n".join(lines)
    doc["line_count"] = len(lines)
    doc["non_empty_lines"] = sum(1 for ln in lines if ln.strip())
    doc["byte_size"] = len(doc["content"].encode("utf-8"))
    return doc


def content_hash(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def exact_dedup(docs):
    """Remove exact duplicates by content hash."""
    seen = {}
    unique = []
    dupes = 0
    for doc in docs:
        h = content_hash(doc["content"])
        if h not in seen:
            seen[h] = doc["source"]
            doc["content_hash"] = h
            unique.append(doc)
        else:
            dupes += 1
    return unique, dupes


def _line_fingerprint(text, max_lines=200):
    """Create a set of normalized line hashes for near-dedup comparison."""
    lines = text.split("\n")[:max_lines]
    fp = set()
    for line in lines:
        stripped = line.strip().lower()
        if len(stripped) > 10:
            fp.add(hashlib.md5(stripped.encode()).digest()[:8])
    return fp


def near_dedup(docs, threshold=0.7):
    """Line-fingerprint near-dedup using bucket + Jaccard check. Fast in pure Python."""
    n = len(docs)
    print(f"  Computing line fingerprints for {n} documents...", flush=True)

    fps = []
    for idx, doc in enumerate(docs):
        if idx % 5000 == 0:
            print(f"    Fingerprints: {idx}/{n}...", flush=True)
        fps.append(_line_fingerprint(doc["content"]))

    print("  Building candidate buckets...", flush=True)
    from collections import defaultdict

    buckets = defaultdict(list)
    for i, fp in enumerate(fps):
        if not fp:
            continue
        size_bucket = len(fp) // 10
        sample = tuple(sorted(fp)[:5])
        for h in sample:
            buckets[(size_bucket, h)].append(i)

    candidates = set()
    for members in buckets.values():
        if 1 < len(members) <= 500:
            for a in range(len(members)):
                for b in range(a + 1, len(members)):
                    i, j = members[a], members[b]
                    if i < j:
                        candidates.add((i, j))
                    else:
                        candidates.add((j, i))

    print(
        f"  Checking {len(candidates)} candidate pairs (from {n * (n - 1) // 2} total)...",
        flush=True,
    )

    removed = set()
    for i, j in candidates:
        if i in removed or j in removed:
            continue
        fp_i, fp_j = fps[i], fps[j]
        if not fp_i or not fp_j:
            continue
        intersection = len(fp_i & fp_j)
        union = len(fp_i | fp_j)
        jacc = intersection / union if union > 0 else 0
        if jacc >= threshold:
            size_i = docs[i].get("byte_size", 0)
            size_j = docs[j].get("byte_size", 0)
            if size_j >= size_i:
                removed.add(i)
            else:
                removed.add(j)

    print(f"  Near dedup removed {len(removed)} documents", flush=True)
    unique = [doc for i, doc in enumerate(docs) if i not in removed]
    return unique, len(removed)


LISTING_PATTERNS = [
    re.compile(r"^1?\s*PP\s+\d+", re.MULTILINE),
    re.compile(r"COMPILER LISTING", re.IGNORECASE),
    re.compile(r"^.*LINE\s+SEQ\s+", re.MULTILINE),
    re.compile(r"^\d{6}\s", re.MULTILINE),
]

BINARY_THRESHOLD = 0.10


def quality_filter(docs, min_non_empty_lines=20):
    """Remove stubs, binary-looking files, compiler listings."""
    passed = []
    filtered = defaultdict(int)

    for doc in docs:
        content = doc["content"]

        if doc.get("non_empty_lines", 0) < min_non_empty_lines:
            filtered["too_short"] += 1
            continue

        null_ratio = content.count("\x00") / max(len(content), 1)
        if null_ratio > BINARY_THRESHOLD:
            filtered["binary"] += 1
            continue

        non_ascii = sum(1 for c in content if ord(c) > 127)
        if non_ascii / max(len(content), 1) > 0.3:
            filtered["non_ascii"] += 1
            continue

        is_listing = False
        for pat in LISTING_PATTERNS:
            matches = pat.findall(content[:2000])
            if len(matches) >= 3:
                is_listing = True
                break
        if is_listing:
            filtered["listing"] += 1
            continue

        lines = content.split("\n")
        hex_lines = sum(
            1
            for ln in lines
            if re.match(r"^[0-9a-fA-F\s:]+$", ln.strip()) and len(ln.strip()) > 40
        )
        if hex_lines > len(lines) * 0.3:
            filtered["hex_data"] += 1
            continue

        passed.append(doc)

    return passed, dict(filtered)


def detect_cobol_structure(content):
    """Detect which COBOL divisions are present."""
    upper = content.upper()
    divisions = []
    for div in ["IDENTIFICATION", "ENVIRONMENT", "DATA", "PROCEDURE"]:
        if f"{div} DIVISION" in upper:
            divisions.append(div)
    has_copybook_markers = (
        "COPY " in upper
        or content.strip().startswith("01 ")
        or content.strip().startswith("05 ")
    )
    return {
        "divisions": divisions,
        "is_complete_program": "IDENTIFICATION" in divisions
        and "PROCEDURE" in divisions,
        "has_data_division": "DATA" in divisions,
        "is_copybook": has_copybook_markers and "PROCEDURE" not in divisions,
    }


def format_for_training(doc):
    """Produce a training record in completion format."""
    content = doc["content"]
    structure = detect_cobol_structure(content)

    record = {
        "text": content,
        "source": doc["source"],
        "type": doc.get("type", "source"),
        "dataset": doc.get("dataset", ""),
        "line_count": doc.get("line_count", 0),
        "byte_size": doc.get("byte_size", 0),
        "content_hash": doc.get("content_hash", ""),
        "is_complete_program": structure["is_complete_program"],
        "is_copybook": structure["is_copybook"],
        "divisions": structure["divisions"],
    }
    return record


def train_val_split(records, val_ratio=0.05):
    """Split records into train/val sets."""
    random.shuffle(records)
    val_size = max(1, int(len(records) * val_ratio))
    return records[val_size:], records[:val_size]


def write_jsonl(records, path):
    with open(path, "w") as f:
        for rec in records:
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")


def main():
    report = {"stages": {}}

    DATA_ROOT = os.path.dirname(RAW)

    print("=== Stage 0: Discover Files ===")
    source_file_paths, jsonl_file_paths = discover_files_from_index(RAW, DATA_ROOT)

    print("\n=== Stage 1: Ingest ===")
    source_files = ingest_source_files_from_paths(RAW, source_file_paths)
    print(f"  Source files: {len(source_files)}")
    jsonl_records = ingest_jsonl_from_paths(RAW, jsonl_file_paths)
    print(f"  JSONL records: {len(jsonl_records)}")
    all_docs = source_files + jsonl_records
    print(f"  Total documents: {len(all_docs)}")
    report["stages"]["ingest"] = {
        "source_files": len(source_files),
        "jsonl_records": len(jsonl_records),
        "total": len(all_docs),
    }

    print("\n=== Stage 2: Normalize ===")
    all_docs = [normalize(doc) for doc in all_docs]
    total_lines = sum(d["line_count"] for d in all_docs)
    total_bytes = sum(d["byte_size"] for d in all_docs)
    print(f"  Total lines: {total_lines:,}")
    print(f"  Total bytes: {total_bytes:,}")
    report["stages"]["normalize"] = {
        "total_lines": total_lines,
        "total_bytes": total_bytes,
    }

    print("\n=== Stage 3: Dedup ===")
    docs_after_exact, exact_dupes = exact_dedup(all_docs)
    print(f"  Exact dedup: removed {exact_dupes}, remaining {len(docs_after_exact)}")

    if len(docs_after_exact) <= 50000:
        docs_after_near, near_dupes = near_dedup(docs_after_exact, threshold=0.7)
        print(f"  Near dedup: removed {near_dupes}, remaining {len(docs_after_near)}")
    else:
        print(f"  Near dedup: skipped (>{50000} docs, would be too slow)")
        docs_after_near = docs_after_exact
        near_dupes = 0

    report["stages"]["dedup"] = {
        "before": len(all_docs),
        "exact_dupes_removed": exact_dupes,
        "near_dupes_removed": near_dupes,
        "after": len(docs_after_near),
    }

    print("\n=== Stage 4: Quality Filter ===")
    docs_filtered, filter_reasons = quality_filter(docs_after_near)
    print(f"  Passed: {len(docs_filtered)}")
    print(f"  Filtered: {sum(filter_reasons.values())} — {dict(filter_reasons)}")
    report["stages"]["quality_filter"] = {
        "before": len(docs_after_near),
        "passed": len(docs_filtered),
        "filtered_reasons": filter_reasons,
    }

    print("\n=== Stage 5: Format ===")
    training_records = [format_for_training(doc) for doc in docs_filtered]

    complete = sum(1 for r in training_records if r["is_complete_program"])
    copybooks = sum(1 for r in training_records if r["is_copybook"])
    fragments = len(training_records) - complete - copybooks
    total_training_lines = sum(r["line_count"] for r in training_records)
    total_training_bytes = sum(r["byte_size"] for r in training_records)
    print(f"  Complete programs: {complete}")
    print(f"  Copybooks: {copybooks}")
    print(f"  Fragments/other: {fragments}")
    print(f"  Total training lines: {total_training_lines:,}")
    print(
        f"  Total training bytes: {total_training_bytes:,} ({total_training_bytes / 1024 / 1024:.1f} MB)"
    )
    report["stages"]["format"] = {
        "total_records": len(training_records),
        "complete_programs": complete,
        "copybooks": copybooks,
        "fragments": fragments,
        "total_lines": total_training_lines,
        "total_bytes": total_training_bytes,
    }

    print("\n=== Stage 6: Train/Val Split ===")
    train_records, val_records = train_val_split(training_records)
    print(f"  Train: {len(train_records)} records")
    print(f"  Val: {len(val_records)} records")

    train_path = os.path.join(OUT, "train.jsonl")
    val_path = os.path.join(OUT, "val.jsonl")
    write_jsonl(train_records, train_path)
    write_jsonl(val_records, val_path)
    print(f"  Written: {train_path}")
    print(f"  Written: {val_path}")

    train_bytes = os.path.getsize(train_path)
    val_bytes = os.path.getsize(val_path)
    print(f"  Train file: {train_bytes / 1024 / 1024:.1f} MB")
    print(f"  Val file: {val_bytes / 1024 / 1024:.1f} MB")

    report["stages"]["split"] = {
        "train_records": len(train_records),
        "val_records": len(val_records),
        "train_bytes": train_bytes,
        "val_bytes": val_bytes,
    }

    sizes = sorted([r["byte_size"] for r in training_records])
    if sizes:
        import statistics

        report["size_distribution"] = {
            "min": sizes[0],
            "median": int(statistics.median(sizes)),
            "mean": int(statistics.mean(sizes)),
            "p90": sizes[int(len(sizes) * 0.9)],
            "p99": sizes[int(len(sizes) * 0.99)],
            "max": sizes[-1],
        }
        print("\n=== Size Distribution (bytes) ===")
        print(f"  Min: {sizes[0]:,}")
        print(f"  Median: {int(statistics.median(sizes)):,}")
        print(f"  P90: {sizes[int(len(sizes) * 0.9)]:,}")
        print(f"  Max: {sizes[-1]:,}")

    with open(REPORT, "w") as f:
        json.dump(report, f, indent=2)
    print(f"\nReport written to {REPORT}")
    print("\n=== Data preparation complete ===")


if __name__ == "__main__":
    main()
