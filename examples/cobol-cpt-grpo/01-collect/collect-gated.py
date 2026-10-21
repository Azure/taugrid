#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Download gated HF datasets: The Stack v1 COBOL, The Stack v2 COBOL, cobol-pretraining-corpus."""

import json
import os
import subprocess
import sys

subprocess.check_call(
    [
        sys.executable,
        "-m",
        "pip",
        "install",
        "-q",
        "huggingface_hub==1.28.0",
        "datasets==5.0.1",
        "pandas==3.0.6",
        "pyarrow==25.0.1",
    ]
)

from datasets import load_dataset  # noqa: E402
from huggingface_hub import HfApi  # noqa: E402

DATA_ROOT = os.environ.get("DATA_ROOT", "/data/datasets/cobol")
RAW = os.path.join(DATA_ROOT, "raw")
os.makedirs(RAW, exist_ok=True)

token = os.environ["HF_TOKEN"]

api = HfApi(token=token)
print(f"Authenticated as: {api.whoami()['name']}", flush=True)


def collect_the_stack_v1():
    out = os.path.join(RAW, "the-stack-v1-cobol")
    os.makedirs(out, exist_ok=True)
    print("\n=== The Stack v1 — COBOL subset ===", flush=True)

    ds = load_dataset(
        "bigcode/the-stack", data_dir="data/cobol", split="train", token=token
    )
    print(f"  Loaded {len(ds)} records", flush=True)

    jsonl_path = os.path.join(out, "the-stack-v1-cobol.jsonl")
    count = 0
    with open(jsonl_path, "w") as f:
        for row in ds:
            f.write(
                json.dumps(
                    {
                        "hexsha": row.get("hexsha", ""),
                        "path": row.get("max_stars_repo_path", row.get("path", "")),
                        "repo": row.get("max_stars_repo_name", ""),
                        "content": row.get("content", ""),
                        "size": row.get("size", 0),
                        "language": "COBOL",
                        "source": "bigcode/the-stack",
                    }
                )
                + "\n"
            )
            count += 1

    print(f"  Saved {count} records to {jsonl_path}", flush=True)

    with open(os.path.join(out, "metadata.json"), "w") as f:
        json.dump(
            {"source": "bigcode/the-stack", "subset": "cobol", "count": count},
            f,
            indent=2,
        )

    src_count = 0
    src_dir = os.path.join(out, "source")
    os.makedirs(src_dir, exist_ok=True)
    ds2 = load_dataset(
        "bigcode/the-stack", data_dir="data/cobol", split="train", token=token
    )
    for i, row in enumerate(ds2):
        content = row.get("content", "")
        if not content.strip():
            continue
        path = row.get("max_stars_repo_path", row.get("path", f"file_{i}.cbl"))
        ext = os.path.splitext(path)[1].lower()
        if ext not in (".cbl", ".cob", ".cpy", ".CBL", ".COB", ".CPY"):
            ext = ".cbl"
        fname = f"{i:06d}_{os.path.basename(path)}"
        if not fname.lower().endswith((".cbl", ".cob", ".cpy")):
            fname += ext
        with open(os.path.join(src_dir, fname), "w") as f:
            f.write(content)
        src_count += 1

    print(f"  Extracted {src_count} source files to {src_dir}", flush=True)


def collect_the_stack_v2():
    out = os.path.join(RAW, "the-stack-v2-cobol")
    os.makedirs(out, exist_ok=True)
    print("\n=== The Stack v2 — COBOL subset ===", flush=True)

    try:
        ds = load_dataset(
            "bigcode/the-stack-v2",
            data_dir="data/cobol",
            split="train",
            token=token,
            trust_remote_code=True,
        )
        print(f"  Loaded {len(ds)} records", flush=True)

        jsonl_path = os.path.join(out, "the-stack-v2-cobol.jsonl")
        count = 0
        with open(jsonl_path, "w") as f:
            for row in ds:
                record = {
                    k: v
                    for k, v in row.items()
                    if isinstance(v, (str, int, float, bool))
                }
                record["source"] = "bigcode/the-stack-v2"
                record["language"] = "COBOL"
                f.write(json.dumps(record) + "\n")
                count += 1

        print(f"  Saved {count} records to {jsonl_path}", flush=True)

        has_content = any(
            row.get("content")
            for row in load_dataset(
                "bigcode/the-stack-v2",
                data_dir="data/cobol",
                split="train[:5]",
                token=token,
                trust_remote_code=True,
            )
        )
        if has_content:
            src_dir = os.path.join(out, "source")
            os.makedirs(src_dir, exist_ok=True)
            src_count = 0
            for i, row in enumerate(ds):
                content = row.get("content", "")
                if not content or not content.strip():
                    continue
                fname = f"{i:06d}.cbl"
                with open(os.path.join(src_dir, fname), "w") as f:
                    f.write(content)
                src_count += 1
            print(f"  Extracted {src_count} source files", flush=True)
        else:
            print(
                "  Note: v2 contains SWHIDs only, no source content (needs Software Heritage S3)",
                flush=True,
            )

        with open(os.path.join(out, "metadata.json"), "w") as f:
            json.dump(
                {
                    "source": "bigcode/the-stack-v2",
                    "subset": "cobol",
                    "count": count,
                    "has_content": has_content,
                },
                f,
                indent=2,
            )

    except Exception as e:
        print(f"  Failed: {e}", flush=True)
        with open(os.path.join(out, "metadata.json"), "w") as f:
            json.dump({"source": "bigcode/the-stack-v2", "error": str(e)}, f, indent=2)


def collect_pretraining_corpus():
    out = os.path.join(RAW, "cobol-pretraining-corpus")
    os.makedirs(out, exist_ok=True)
    print("\n=== 4sp1d3r2/cobol-pretraining-corpus ===", flush=True)

    ds = load_dataset("4sp1d3r2/cobol-pretraining-corpus", split="train", token=token)
    print(f"  Loaded {len(ds)} records", flush=True)
    print(f"  Columns: {ds.column_names}", flush=True)

    jsonl_path = os.path.join(out, "cobol-pretraining-corpus.jsonl")
    count = 0
    with open(jsonl_path, "w") as f:
        for row in ds:
            record = {
                k: v for k, v in row.items() if isinstance(v, (str, int, float, bool))
            }
            record["source"] = "4sp1d3r2/cobol-pretraining-corpus"
            f.write(json.dumps(record) + "\n")
            count += 1

    print(f"  Saved {count} records to {jsonl_path}", flush=True)

    with open(os.path.join(out, "metadata.json"), "w") as f:
        json.dump(
            {
                "source": "4sp1d3r2/cobol-pretraining-corpus",
                "count": count,
                "columns": ds.column_names,
            },
            f,
            indent=2,
        )


if __name__ == "__main__":
    print("=== Gated COBOL dataset collection ===", flush=True)
    print(f"  Output: {RAW}", flush=True)

    collect_the_stack_v1()
    collect_the_stack_v2()
    collect_pretraining_corpus()

    print("\n=== Collection complete ===", flush=True)
