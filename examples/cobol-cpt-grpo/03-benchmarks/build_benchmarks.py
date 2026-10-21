#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""
Build COBOL evaluation benchmarks from the prepared validation set.

Reads val.jsonl and constructs four benchmark suites:
  1. paragraph_completion — mask a PROCEDURE DIVISION paragraph body, predict it
  2. copybook_usage — given a copybook + partial program, predict field references
  3. program_completion — mask PROCEDURE DIVISION of a complete program
  4. perplexity — held-out complete programs for perplexity measurement

Each benchmark is a JSONL file with prompt/reference/metadata fields.
Writes to /data/datasets/cobol/eval/ on the PVC.
"""

import json
import os
import re
import random
import hashlib

VAL_PATH = os.environ.get("VAL", "/data/datasets/cobol/prepared/val.jsonl")
TRAIN_PATH = os.environ.get("TRAIN", "/data/datasets/cobol/prepared/train.jsonl")
OUT = os.environ.get("OUT", "/data/datasets/cobol/eval")
SEED = 42

os.makedirs(OUT, exist_ok=True)
random.seed(SEED)

PARA_RE = re.compile(r"^       (\d{4}-[A-Z0-9-]+|[A-Z][A-Z0-9-]*)\.\s*$", re.MULTILINE)

DIVISION_RE = re.compile(
    r"(IDENTIFICATION|ENVIRONMENT|DATA|PROCEDURE)\s+DIVISION", re.IGNORECASE
)


def load_jsonl(path):
    records = []
    with open(path, "r") as f:
        for line in f:
            line = line.strip()
            if line:
                records.append(json.loads(line))
    return records


def split_divisions(text):
    """Split COBOL source into divisions."""
    divisions = {}
    parts = DIVISION_RE.split(text)
    if len(parts) < 2:
        return divisions
    i = 1
    while i < len(parts):
        div_name = parts[i].upper()
        if i + 1 < len(parts):
            div_body = parts[i + 1]
            next_div = DIVISION_RE.search(div_body)
            if next_div:
                div_body = div_body[: next_div.start()]
            divisions[div_name] = div_body
        i += 2
    return divisions


def extract_paragraphs(procedure_text):
    """Extract named paragraphs from PROCEDURE DIVISION text."""
    paragraphs = []
    matches = list(PARA_RE.finditer(procedure_text))
    for idx, match in enumerate(matches):
        name = match.group(1)
        start = match.end()
        end = (
            matches[idx + 1].start() if idx + 1 < len(matches) else len(procedure_text)
        )
        body = procedure_text[start:end].rstrip()
        if body.strip():
            paragraphs.append(
                {"name": name, "body": body, "start": match.start(), "end": end}
            )
    return paragraphs


def build_paragraph_completion(records, max_tasks=200):
    """Build paragraph completion benchmark.

    Prompt: everything before the paragraph body (divisions + paragraph name)
    Reference: the paragraph body
    """
    tasks = []
    for rec in records:
        if not rec.get("is_complete_program"):
            continue
        text = rec["text"]
        divisions = split_divisions(text)
        if "PROCEDURE" not in divisions:
            continue

        proc_text = divisions["PROCEDURE"]
        paragraphs = extract_paragraphs(proc_text)

        good_paras = [
            p
            for p in paragraphs
            if len([ln for ln in p["body"].split("\n") if ln.strip()]) >= 3
        ]
        if not good_paras:
            continue

        para = random.choice(good_paras)

        proc_div_header = "       PROCEDURE DIVISION"
        proc_prefix = proc_text[: para["start"]]
        non_proc = (
            text.split("PROCEDURE DIVISION")[0]
            if "PROCEDURE DIVISION" in text.upper()
            else ""
        )

        prompt = non_proc.rstrip() + "\n" + proc_div_header + proc_prefix
        prompt = prompt.rstrip() + "\n       " + para["name"] + ".\n"

        reference = para["body"]

        tasks.append(
            {
                "id": f"para-{hashlib.md5(prompt.encode()).hexdigest()[:8]}",
                "type": "paragraph_completion",
                "source": rec.get("source", ""),
                "paragraph_name": para["name"],
                "prompt": prompt,
                "reference": reference,
                "reference_lines": len(
                    [ln for ln in reference.split("\n") if ln.strip()]
                ),
                "stop_at": [
                    "\n       " + p["name"] + "."
                    for p in paragraphs
                    if p["name"] != para["name"]
                ][:5],
            }
        )

    random.shuffle(tasks)
    return tasks[:max_tasks]


def build_copybook_usage(records, max_tasks=100):
    """Build copybook usage benchmark.

    Find programs that reference copybook-like data structures, then mask
    statements that use those fields.
    """
    copybooks = [r for r in records if r.get("is_copybook")]
    programs = [r for r in records if r.get("is_complete_program")]

    if not copybooks or not programs:
        return []

    field_re = re.compile(r"^\s+\d{2}\s+([A-Z][A-Z0-9-]+)\s", re.MULTILINE)
    copybook_fields = {}
    for cb in copybooks:
        fields = field_re.findall(cb["text"])
        if len(fields) >= 3:
            copybook_fields[cb.get("source", "")] = {
                "text": cb["text"],
                "fields": fields,
            }

    tasks = []
    for prog in programs:
        text = prog["text"]
        divisions = split_divisions(text)
        if "PROCEDURE" not in divisions or "DATA" not in divisions:
            continue

        proc_text = divisions["PROCEDURE"]

        for cb_src, cb_info in copybook_fields.items():
            used_fields = [f for f in cb_info["fields"] if f in proc_text]
            if len(used_fields) < 2:
                continue

            lines = proc_text.split("\n")
            field_lines = []
            for i, line in enumerate(lines):
                if any(f in line for f in used_fields):
                    field_lines.append((i, line))

            if len(field_lines) < 2:
                continue

            start_idx = field_lines[0][0]
            end_idx = min(start_idx + 8, field_lines[-1][0] + 1)
            masked_lines = lines[start_idx:end_idx]
            reference = "\n".join(masked_lines)

            context_before = "\n".join(lines[:start_idx])
            context_after = "\n".join(lines[end_idx : end_idx + 5])

            prompt = (
                "Given this copybook:\n"
                + cb_info["text"].strip()
                + "\n\nAnd this partial COBOL program:\n"
                + text.split("PROCEDURE DIVISION")[0].rstrip()
                + "\n       PROCEDURE DIVISION.\n"
                + context_before.rstrip()
                + "\n           [COMPLETE THE FOLLOWING LINES]\n"
            )

            tasks.append(
                {
                    "id": f"cpyb-{hashlib.md5(prompt.encode()).hexdigest()[:8]}",
                    "type": "copybook_usage",
                    "source": prog.get("source", ""),
                    "copybook_source": cb_src,
                    "copybook_fields_used": used_fields,
                    "prompt": prompt,
                    "reference": reference,
                    "context_after": context_after,
                    "reference_lines": len(masked_lines),
                }
            )
            break

    random.shuffle(tasks)
    return tasks[:max_tasks]


def build_program_completion(records, max_tasks=50):
    """Build full PROCEDURE DIVISION completion benchmark.

    Prompt: IDENTIFICATION + ENVIRONMENT + DATA divisions
    Reference: PROCEDURE DIVISION
    """
    tasks = []
    for rec in records:
        if not rec.get("is_complete_program"):
            continue
        text = rec["text"]

        divisions = split_divisions(text)
        if "PROCEDURE" not in divisions:
            continue

        proc_div = divisions["PROCEDURE"]
        proc_lines = [ln for ln in proc_div.split("\n") if ln.strip()]
        if len(proc_lines) < 10 or len(proc_lines) > 200:
            continue

        upper = text.upper()
        proc_idx = upper.find("PROCEDURE DIVISION")
        if proc_idx < 0:
            continue

        prompt = text[:proc_idx].rstrip() + "\n       PROCEDURE DIVISION.\n"
        reference = proc_div

        tasks.append(
            {
                "id": f"prog-{hashlib.md5(prompt.encode()).hexdigest()[:8]}",
                "type": "program_completion",
                "source": rec.get("source", ""),
                "prompt": prompt,
                "reference": reference,
                "reference_lines": len(proc_lines),
                "total_program_lines": rec.get("line_count", 0),
            }
        )

    random.shuffle(tasks)
    return tasks[:max_tasks]


def build_perplexity_set(records, max_docs=500):
    """Build perplexity evaluation set — complete programs for loss measurement."""
    complete = [
        r
        for r in records
        if r.get("is_complete_program") and r.get("line_count", 0) >= 30
    ]
    random.shuffle(complete)
    tasks = []
    for rec in complete[:max_docs]:
        tasks.append(
            {
                "id": f"ppl-{hashlib.md5(rec['text'].encode()).hexdigest()[:8]}",
                "type": "perplexity",
                "source": rec.get("source", ""),
                "text": rec["text"],
                "line_count": rec.get("line_count", 0),
                "byte_size": rec.get("byte_size", 0),
            }
        )
    return tasks


def build_structural_validation_set(records, max_tasks=100):
    """Build set for structural validation — programs to test compilation and formatting."""
    complete = [
        r
        for r in records
        if r.get("is_complete_program") and 30 <= r.get("line_count", 0) <= 500
    ]
    random.shuffle(complete)
    tasks = []
    for rec in complete[:max_tasks]:
        text = rec["text"]
        divisions = split_divisions(text)
        if "PROCEDURE" not in divisions:
            continue

        upper = text.upper()
        proc_idx = upper.find("PROCEDURE DIVISION")
        if proc_idx < 0:
            continue

        prompt = text[:proc_idx].rstrip() + "\n       PROCEDURE DIVISION.\n"

        tasks.append(
            {
                "id": f"strv-{hashlib.md5(prompt.encode()).hexdigest()[:8]}",
                "type": "structural_validation",
                "source": rec.get("source", ""),
                "prompt": prompt,
                "reference_text": text,
                "reference_divisions": list(divisions.keys()),
                "line_count": rec.get("line_count", 0),
            }
        )
    return tasks


def write_jsonl(records, path):
    with open(path, "w") as f:
        for rec in records:
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")
    print(f"  Written {len(records)} tasks to {path}", flush=True)


def main():
    print("=== Building COBOL Evaluation Benchmarks ===", flush=True)

    print(f"\nLoading validation set from {VAL_PATH}...", flush=True)
    val_records = load_jsonl(VAL_PATH)
    print(f"  Loaded {len(val_records)} validation records", flush=True)

    print(f"Loading training set from {TRAIN_PATH}...", flush=True)
    train_records = load_jsonl(TRAIN_PATH)
    print(f"  Loaded {len(train_records)} training records", flush=True)

    random.seed(SEED)
    all_records = val_records

    print("\n--- Benchmark 1: Paragraph Completion ---", flush=True)
    para_tasks = build_paragraph_completion(all_records, max_tasks=200)
    print(f"  Generated {len(para_tasks)} paragraph completion tasks", flush=True)
    if para_tasks:
        avg_lines = sum(t["reference_lines"] for t in para_tasks) / len(para_tasks)
        print(f"  Average reference lines: {avg_lines:.1f}", flush=True)
    write_jsonl(para_tasks, os.path.join(OUT, "paragraph_completion.jsonl"))

    print("\n--- Benchmark 2: Copybook Usage ---", flush=True)
    cpyb_pool = val_records + [r for r in train_records if r.get("is_copybook")]
    cpyb_tasks = build_copybook_usage(cpyb_pool, max_tasks=100)
    print(f"  Generated {len(cpyb_tasks)} copybook usage tasks", flush=True)
    write_jsonl(cpyb_tasks, os.path.join(OUT, "copybook_usage.jsonl"))

    print("\n--- Benchmark 3: Program Completion ---", flush=True)
    prog_tasks = build_program_completion(all_records, max_tasks=50)
    print(f"  Generated {len(prog_tasks)} program completion tasks", flush=True)
    if prog_tasks:
        avg_lines = sum(t["reference_lines"] for t in prog_tasks) / len(prog_tasks)
        print(f"  Average PROCEDURE DIVISION lines: {avg_lines:.1f}", flush=True)
    write_jsonl(prog_tasks, os.path.join(OUT, "program_completion.jsonl"))

    print("\n--- Benchmark 4: Perplexity ---", flush=True)
    ppl_tasks = build_perplexity_set(all_records, max_docs=len(all_records))
    print(f"  Generated {len(ppl_tasks)} perplexity evaluation docs", flush=True)
    write_jsonl(ppl_tasks, os.path.join(OUT, "perplexity.jsonl"))

    print("\n--- Benchmark 5: Structural Validation ---", flush=True)
    strv_tasks = build_structural_validation_set(all_records, max_tasks=100)
    print(f"  Generated {len(strv_tasks)} structural validation tasks", flush=True)
    write_jsonl(strv_tasks, os.path.join(OUT, "structural_validation.jsonl"))

    total = (
        len(para_tasks)
        + len(cpyb_tasks)
        + len(prog_tasks)
        + len(ppl_tasks)
        + len(strv_tasks)
    )
    summary = {
        "benchmarks": {
            "paragraph_completion": {
                "count": len(para_tasks),
                "file": "paragraph_completion.jsonl",
            },
            "copybook_usage": {
                "count": len(cpyb_tasks),
                "file": "copybook_usage.jsonl",
            },
            "program_completion": {
                "count": len(prog_tasks),
                "file": "program_completion.jsonl",
            },
            "perplexity": {"count": len(ppl_tasks), "file": "perplexity.jsonl"},
            "structural_validation": {
                "count": len(strv_tasks),
                "file": "structural_validation.jsonl",
            },
        },
        "total_tasks": total,
        "source": VAL_PATH,
    }
    with open(os.path.join(OUT, "benchmark-summary.json"), "w") as f:
        json.dump(summary, f, indent=2)
    print(
        f"\n=== Benchmark suite complete: {total} total tasks across 5 benchmarks ===",
        flush=True,
    )


if __name__ == "__main__":
    main()
