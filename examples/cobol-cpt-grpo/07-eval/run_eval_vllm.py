#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""
COBOL evaluation harness — vLLM backend for multi-GPU batch inference.

Uses vLLM with tensor parallelism across all available GPUs on a node.
All benchmark tasks are batched and processed concurrently.

Before loading the model, main() fails unless every suite in REQUIRED_SUITES
exists in EVAL_DIR with at least one task that has the fields in REQUIRED_FIELDS
and `cobc --version` succeeds.

Paragraph completions are cut at the next paragraph header and compiled with
the rest of the original procedure appended. Each compile suite also reports
how many tasks compile with the reference, and with an empty generation, in
place of the generation.
"""

import json
import math
import os
import re
import subprocess
import tempfile
from collections import defaultdict

import torch
from vllm import LLM, SamplingParams

EVAL_DIR = os.environ.get("EVAL_DIR", "/data/datasets/cobol/eval")
MODEL_ID = os.environ.get("MODEL_ID", "Qwen/Qwen2.5-Coder-32B")
OUT = os.environ.get("OUT", "/data/datasets/cobol/eval/results/baseline")
MAX_NEW_TOKENS = int(os.environ.get("MAX_NEW_TOKENS", "512"))
MAX_MODEL_LEN = int(os.environ.get("MAX_MODEL_LEN", "32768"))
TP_SIZE = int(os.environ.get("TP_SIZE", str(torch.cuda.device_count())))
HF_HOME = os.environ.get("HF_HOME", "/tmp/hf-cache")

os.makedirs(OUT, exist_ok=True)
os.environ["HF_HOME"] = HF_HOME


REQUIRED_SUITES = (
    "perplexity",
    "paragraph_completion",
    "program_completion",
    "structural_validation",
    "copybook_usage",
)

REQUIRED_FIELDS = {
    "perplexity": ("id", "text"),
    "paragraph_completion": ("id", "prompt", "reference", "suffix"),
    "program_completion": ("id", "prompt", "reference", "reference_text"),
    "structural_validation": ("id", "prompt", "reference_text"),
    "copybook_usage": ("id", "prompt", "reference"),
}

PARA_RE = re.compile(r"^       (\d{4}-[A-Z0-9-]+|[A-Z][A-Z0-9-]*)\.\s*$", re.MULTILINE)

GENERATION_BUDGETS = {
    "paragraph_completion": MAX_NEW_TOKENS,
    "program_completion": 1024,
    "structural_validation": 1024,
    "copybook_usage": MAX_NEW_TOKENS,
}


def fit_to_context(suites, tokenizer, max_model_len):
    """Set aside generation tasks whose prompt plus output budget exceeds the context."""
    excluded = {}
    emptied = []
    for name, budget in GENERATION_BUDGETS.items():
        limit = max_model_len - budget
        kept = []
        too_long = []
        for task in suites[name]:
            n_tokens = len(tokenizer(task["prompt"])["input_ids"])
            if n_tokens > limit:
                too_long.append({"id": task["id"], "prompt_tokens": n_tokens})
            else:
                kept.append(task)
        if too_long:
            excluded[name] = {"prompt_token_limit": limit, "tasks": too_long}
        if not kept:
            emptied.append(name)
        suites[name] = kept
    if emptied:
        raise RuntimeError(
            f"No task fits in max_model_len={max_model_len} for: {', '.join(emptied)}"
        )
    return excluded


def check_compiler():
    try:
        result = subprocess.run(
            ["cobc", "--version"], capture_output=True, text=True, timeout=10
        )
    except FileNotFoundError:
        raise RuntimeError("cobc not found; GnuCOBOL must be installed") from None
    except subprocess.TimeoutExpired:
        raise RuntimeError("cobc --version timed out") from None
    if result.returncode != 0:
        raise RuntimeError(f"cobc --version exited with code {result.returncode}")
    return result.stdout.splitlines()[0] if result.stdout else ""


def load_required_suites(eval_dir):
    suites = {}
    problems = []
    for name in REQUIRED_SUITES:
        path = os.path.join(eval_dir, f"{name}.jsonl")
        if not os.path.isfile(path):
            problems.append(f"{path} is missing")
            continue
        tasks = load_jsonl(path)
        if not tasks:
            problems.append(f"{path} has no tasks")
            continue
        missing = sorted(
            {f for task in tasks for f in REQUIRED_FIELDS[name] if f not in task}
        )
        if missing:
            problems.append(f"{path} tasks lack {', '.join(missing)}")
            continue
        suites[name] = tasks
    if problems:
        raise RuntimeError(
            "Benchmark suites are incomplete; run 03-benchmarks first: "
            + "; ".join(problems)
        )
    return suites


def load_jsonl(path):
    records = []
    if not os.path.isfile(path):
        return records
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                records.append(json.loads(line))
    return records


def eval_perplexity(tasks, max_length=4096):
    """Compute perplexity via HuggingFace model (teacher-forcing, not generation)."""
    from transformers import AutoTokenizer, AutoModelForCausalLM

    print(f"\n=== Perplexity Evaluation ({len(tasks)} docs) ===", flush=True)
    print("  Loading HF model for perplexity (single GPU)...", flush=True)

    tokenizer = AutoTokenizer.from_pretrained(MODEL_ID)
    n_gpus = torch.cuda.device_count()
    max_mem = {i: "17GiB" for i in range(n_gpus)}
    model = AutoModelForCausalLM.from_pretrained(
        MODEL_ID,
        torch_dtype=torch.bfloat16,
        device_map="auto",
        max_memory=max_mem,
    )

    total_nll = 0.0
    total_tokens = 0
    per_doc = []

    model.eval()
    with torch.no_grad():
        for idx, task in enumerate(tasks):
            if idx % 20 == 0:
                print(f"  Progress: {idx}/{len(tasks)}...", flush=True)

            text = task["text"]
            encodings = tokenizer(
                text, return_tensors="pt", truncation=True, max_length=max_length
            )
            input_ids = encodings.input_ids.to(model.device)

            if input_ids.shape[1] < 2:
                continue

            outputs = model(input_ids, labels=input_ids)
            nll = outputs.loss.item() * (input_ids.shape[1] - 1)
            n_tokens = input_ids.shape[1] - 1

            total_nll += nll
            total_tokens += n_tokens
            doc_ppl = math.exp(nll / n_tokens) if n_tokens > 0 else float("inf")
            per_doc.append(
                {
                    "id": task["id"],
                    "source": task.get("source", ""),
                    "perplexity": round(doc_ppl, 2),
                    "tokens": n_tokens,
                }
            )

    avg_ppl = math.exp(total_nll / total_tokens) if total_tokens > 0 else float("inf")
    print(
        f"  Aggregate perplexity: {avg_ppl:.2f} ({total_tokens:,} tokens)", flush=True
    )

    del model
    del tokenizer
    torch.cuda.empty_cache()

    return {
        "aggregate_perplexity": round(avg_ppl, 2),
        "total_tokens": total_tokens,
        "num_docs": len(per_doc),
        "per_doc": per_doc,
    }


def validate_division_structure(text):
    upper = text.upper()
    errors = []
    if "IDENTIFICATION DIVISION" not in upper:
        errors.append("missing_identification_division")
    if "PROCEDURE DIVISION" not in upper:
        errors.append("missing_procedure_division")

    positions = {}
    for div in ["IDENTIFICATION", "ENVIRONMENT", "DATA", "PROCEDURE"]:
        idx = upper.find(f"{div} DIVISION")
        if idx >= 0:
            positions[div] = idx

    sorted_divs = sorted(positions.items(), key=lambda x: x[1])
    expected_order = ["IDENTIFICATION", "ENVIRONMENT", "DATA", "PROCEDURE"]
    actual_order = [d for d, _ in sorted_divs]
    if actual_order != [d for d in expected_order if d in positions]:
        errors.append("wrong_division_order")

    return errors


def validate_column_formatting(text):
    errors = []
    lines = text.split("\n")
    violations = 0
    total_code_lines = 0

    for line in lines:
        if len(line) < 7:
            continue
        total_code_lines += 1
        col7 = line[6] if len(line) > 6 else " "
        if col7 not in " */-dD":
            violations += 1
        if len(line) > 72:
            content_past_72 = line[72:].strip()
            if content_past_72 and not line[72:].strip().isdigit():
                violations += 1

    violation_rate = violations / max(total_code_lines, 1)
    if violation_rate > 0.1:
        errors.append(f"column_violations_{violation_rate:.0%}")

    return errors, violations, total_code_lines


def validate_pic_clauses(text):
    pic_re = re.compile(
        r"PIC(?:TURE)?\s+IS\s+([^\s.]+)|PIC(?:TURE)?\s+([^\s.]+)", re.IGNORECASE
    )
    matches = pic_re.findall(text)
    valid = 0
    invalid = 0
    for m in matches:
        pic_str = m[0] or m[1]
        if re.match(r"^[X9AZSV()\d+\-.]+$", pic_str, re.IGNORECASE):
            valid += 1
        else:
            invalid += 1
    return valid, invalid


def validate_paragraph_refs(text):
    para_def_re = re.compile(r"^       ([A-Z0-9][A-Z0-9-]*)\.\s*$", re.MULTILINE)
    defined = set(m.group(1) for m in para_def_re.finditer(text))

    perform_re = re.compile(r"PERFORM\s+([A-Z0-9][A-Z0-9-]+)", re.IGNORECASE)
    referenced = set()
    dangling = set()
    for m in perform_re.finditer(text):
        target = m.group(1).upper()
        if target in ("UNTIL", "VARYING", "TIMES", "TEST", "WITH"):
            continue
        referenced.add(target)
        if target not in {d.upper() for d in defined}:
            dangling.add(target)

    return len(referenced), len(dangling), defined


def eval_structural(tasks, generated_texts):
    print(f"\n=== Structural Validation ({len(tasks)} tasks) ===", flush=True)
    results = []
    totals = defaultdict(int)

    for task, gen_text in zip(tasks, generated_texts):
        full_text = task["prompt"] + gen_text

        div_errors = validate_division_structure(full_text)
        col_errors, col_violations, col_total = validate_column_formatting(full_text)
        pic_valid, pic_invalid = validate_pic_clauses(full_text)
        ref_total, ref_dangling, _ = validate_paragraph_refs(full_text)

        totals["division_valid"] += 1 if not div_errors else 0
        totals["column_valid"] += 1 if not col_errors else 0
        totals["pic_valid"] += pic_valid
        totals["pic_invalid"] += pic_invalid
        totals["ref_total"] += ref_total
        totals["ref_dangling"] += ref_dangling
        totals["total"] += 1

        results.append(
            {
                "id": task["id"],
                "division_errors": div_errors,
                "column_violations": col_violations,
                "column_total_lines": col_total,
                "pic_valid": pic_valid,
                "pic_invalid": pic_invalid,
                "ref_total": ref_total,
                "ref_dangling": ref_dangling,
            }
        )

    n = max(totals["total"], 1)
    summary = {
        "division_structure_valid_pct": round(totals["division_valid"] / n * 100, 1),
        "column_format_valid_pct": round(totals["column_valid"] / n * 100, 1),
        "pic_accuracy_pct": round(
            totals["pic_valid"]
            / max(totals["pic_valid"] + totals["pic_invalid"], 1)
            * 100,
            1,
        ),
        "perform_ref_accuracy_pct": round(
            (totals["ref_total"] - totals["ref_dangling"])
            / max(totals["ref_total"], 1)
            * 100,
            1,
        ),
        "num_tasks": n,
    }
    print(
        f"  Division structure valid: {summary['division_structure_valid_pct']}%",
        flush=True,
    )
    print(
        f"  Column formatting valid: {summary['column_format_valid_pct']}%", flush=True
    )
    print(f"  PIC clause accuracy: {summary['pic_accuracy_pct']}%", flush=True)
    print(
        f"  PERFORM reference accuracy: {summary['perform_ref_accuracy_pct']}%",
        flush=True,
    )

    return {"summary": summary, "details": results}


def _compile_once(src_path, fmt_flag):
    try:
        result = subprocess.run(
            ["cobc", "-fsyntax-only", fmt_flag, src_path],
            capture_output=True,
            text=True,
            errors="replace",
            timeout=10,
        )
        success = result.returncode == 0
        errors = result.stderr.strip() if not success else ""
        return success, errors
    except FileNotFoundError:
        return None, "cobc not found"
    except subprocess.TimeoutExpired:
        return False, "compilation timeout"


def try_compile(cobol_text):
    with tempfile.NamedTemporaryFile(suffix=".cbl", mode="w", delete=False) as f:
        f.write(cobol_text)
        f.flush()
        src_path = f.name

    try:
        success, errors = _compile_once(src_path, "-free")
        fmt_used = "free"
        if not success:
            success_fixed, errors_fixed = _compile_once(src_path, "-fixed")
            if success_fixed:
                success, errors, fmt_used = True, "", "fixed"
            elif errors_fixed and len(errors_fixed) < len(errors):
                errors = errors_fixed
                fmt_used = "fixed"
        return success, errors, fmt_used
    finally:
        os.unlink(src_path)


def cut_at_next_paragraph(text):
    """Keep a paragraph completion up to the first paragraph header it starts."""
    match = PARA_RE.search(text)
    return text[: match.start()] if match else text


def eval_compilation(tasks, generated_texts, reference_texts):
    print(f"\n=== Compilation Evaluation ({len(tasks)} tasks) ===", flush=True)
    compiled = 0
    failed = 0
    reference_compiled = 0
    compiled_where_reference_compiles = 0
    empty_compiled = 0
    fmt_counts = {"free": 0, "fixed": 0}
    results = []

    for task, gen_text, ref_text in zip(tasks, generated_texts, reference_texts):
        suffix = task.get("suffix", "")
        success, errors, fmt_used = try_compile(task["prompt"] + gen_text + suffix)
        ref_success, _, _ = try_compile(ref_text)
        empty_success, _, _ = try_compile(task["prompt"] + suffix)
        if None in (success, ref_success, empty_success):
            raise RuntimeError(f"cobc not found while compiling task {task['id']}")
        if empty_success:
            empty_compiled += 1
        if success:
            compiled += 1
            fmt_counts[fmt_used] += 1
        else:
            failed += 1
        if ref_success:
            reference_compiled += 1
            if success:
                compiled_where_reference_compiles += 1

        results.append(
            {
                "id": task["id"],
                "compiled": success,
                "reference_compiled": ref_success,
                "empty_compiled": empty_success,
                "errors": errors[:500] if errors else "",
                "format": fmt_used,
            }
        )

    total = compiled + failed
    rate = compiled / max(total, 1) * 100
    print(f"  Compiled: {compiled}/{total} ({rate:.1f}%)", flush=True)
    print(
        f"  Format breakdown: {fmt_counts['free']} free, {fmt_counts['fixed']} fixed",
        flush=True,
    )
    print(
        f"  Reference compiles: {reference_compiled}/{total}; generation compiles "
        f"on {compiled_where_reference_compiles} of those",
        flush=True,
    )
    print(f"  Empty generation compiles: {empty_compiled}/{total}", flush=True)

    return {
        "compilation_rate_pct": round(rate, 1),
        "compiled": compiled,
        "failed": failed,
        "compiled_free": fmt_counts["free"],
        "compiled_fixed": fmt_counts["fixed"],
        "reference_compiled": reference_compiled,
        "compiled_where_reference_compiles": compiled_where_reference_compiles,
        "empty_compiled": empty_compiled,
        "details": results,
    }


def normalized_edit_distance(s1, s2):
    if not s1 and not s2:
        return 0.0
    if not s1 or not s2:
        return 1.0

    m, n = len(s1), len(s2)
    if m > 2000 or n > 2000:
        s1, s2 = s1[:2000], s2[:2000]
        m, n = len(s1), len(s2)

    dp = list(range(n + 1))
    for i in range(1, m + 1):
        prev = dp[0]
        dp[0] = i
        for j in range(1, n + 1):
            temp = dp[j]
            if s1[i - 1] == s2[j - 1]:
                dp[j] = prev
            else:
                dp[j] = 1 + min(dp[j], dp[j - 1], prev)
            prev = temp

    return dp[n] / max(m, n)


def compute_bleu(reference, hypothesis, max_n=4):
    ref_tokens = reference.split()
    hyp_tokens = hypothesis.split()

    if not hyp_tokens or not ref_tokens:
        return 0.0

    precisions = []
    for n in range(1, max_n + 1):
        ref_ngrams = defaultdict(int)
        for i in range(len(ref_tokens) - n + 1):
            ngram = tuple(ref_tokens[i : i + n])
            ref_ngrams[ngram] += 1

        matches = 0
        total = 0
        for i in range(len(hyp_tokens) - n + 1):
            ngram = tuple(hyp_tokens[i : i + n])
            if ref_ngrams.get(ngram, 0) > 0:
                matches += 1
                ref_ngrams[ngram] -= 1
            total += 1

        if total == 0:
            precisions.append(0)
        else:
            precisions.append(matches / total)

    if any(p == 0 for p in precisions):
        return 0.0

    log_avg = sum(math.log(p) for p in precisions) / max_n

    bp = 1.0
    if len(hyp_tokens) < len(ref_tokens):
        bp = math.exp(1 - len(ref_tokens) / max(len(hyp_tokens), 1))

    return bp * math.exp(log_avg)


def eval_generation_quality(tasks, generated_texts, benchmark_name):
    print(
        f"\n=== Generation Quality: {benchmark_name} ({len(tasks)} tasks) ===",
        flush=True,
    )
    bleu_scores = []
    edit_dists = []
    results = []

    for task, gen_text in zip(tasks, generated_texts):
        reference = task["reference"]
        bleu = compute_bleu(reference, gen_text)
        ed = normalized_edit_distance(reference.strip(), gen_text.strip())

        bleu_scores.append(bleu)
        edit_dists.append(ed)
        results.append(
            {
                "id": task["id"],
                "bleu": round(bleu, 4),
                "edit_distance": round(ed, 4),
            }
        )

    avg_bleu = sum(bleu_scores) / max(len(bleu_scores), 1)
    avg_ed = sum(edit_dists) / max(len(edit_dists), 1)
    print(f"  Average BLEU: {avg_bleu:.4f}", flush=True)
    print(f"  Average normalized edit distance: {avg_ed:.4f}", flush=True)

    return {
        "avg_bleu": round(avg_bleu, 4),
        "avg_edit_distance": round(avg_ed, 4),
        "num_tasks": len(tasks),
        "details": results,
    }


def batch_generate(llm, tasks, max_new_tokens):
    """Generate completions for all tasks in one batched vLLM call."""
    prompts = [task["prompt"] for task in tasks]
    sampling_params = SamplingParams(
        max_tokens=max_new_tokens,
        temperature=0,
        top_p=1.0,
    )
    print(
        f"  Generating {len(prompts)} completions (batch, TP={TP_SIZE})...", flush=True
    )
    outputs = llm.generate(prompts, sampling_params)
    generated = [output.outputs[0].text for output in outputs]
    print(f"  Done — {len(generated)} completions generated.", flush=True)
    return generated


def main():
    print("=== COBOL Evaluation Harness (vLLM) ===", flush=True)
    print(f"  Model: {MODEL_ID}", flush=True)
    print(f"  GPUs: {torch.cuda.device_count()}, TP size: {TP_SIZE}", flush=True)
    print(f"  Eval dir: {EVAL_DIR}", flush=True)

    print(f"  Compiler: {check_compiler()}", flush=True)
    suites = load_required_suites(EVAL_DIR)
    from transformers import AutoTokenizer

    excluded = fit_to_context(
        suites, AutoTokenizer.from_pretrained(MODEL_ID), MAX_MODEL_LEN
    )
    for name in REQUIRED_SUITES:
        dropped = len(excluded.get(name, {}).get("tasks", []))
        note = f" ({dropped} over the context limit set aside)" if dropped else ""
        print(f"  {name}: {len(suites[name])} tasks{note}", flush=True)

    all_results = {
        "model": MODEL_ID,
        "backend": "vllm",
        "tp_size": TP_SIZE,
        "max_model_len": MAX_MODEL_LEN,
        "excluded_over_context": excluded,
    }

    ppl_tasks = suites["perplexity"]
    if ppl_tasks:
        ppl_results = eval_perplexity(ppl_tasks)
        all_results["perplexity"] = {
            k: v for k, v in ppl_results.items() if k != "per_doc"
        }
        with open(os.path.join(OUT, "perplexity_details.jsonl"), "w") as f:
            for doc in ppl_results.get("per_doc", []):
                f.write(json.dumps(doc) + "\n")

    print(f"\nLoading vLLM model (TP={TP_SIZE})...", flush=True)
    llm = LLM(
        model=MODEL_ID,
        tensor_parallel_size=TP_SIZE,
        dtype="bfloat16",
        max_model_len=MAX_MODEL_LEN,
        gpu_memory_utilization=0.90,
        download_dir=HF_HOME,
        distributed_executor_backend="mp",
    )
    print("  vLLM model loaded.", flush=True)

    para_tasks = suites["paragraph_completion"]
    if para_tasks:
        print(
            f"\n--- Paragraph completion ({len(para_tasks)} tasks, max_tokens={MAX_NEW_TOKENS}) ---",
            flush=True,
        )
        para_gen = [
            cut_at_next_paragraph(g)
            for g in batch_generate(llm, para_tasks, max_new_tokens=MAX_NEW_TOKENS)
        ]
        para_quality = eval_generation_quality(
            para_tasks, para_gen, "paragraph_completion"
        )
        all_results["paragraph_completion"] = para_quality
        para_compile = eval_compilation(
            [
                {"id": t["id"], "prompt": t["prompt"], "suffix": t["suffix"]}
                for t in para_tasks
            ],
            para_gen,
            [t["prompt"] + t["reference"] + t["suffix"] for t in para_tasks],
        )
        all_results["paragraph_compilation"] = {
            k: v for k, v in para_compile.items() if k != "details"
        }
        with open(os.path.join(OUT, "paragraph_compilation_details.jsonl"), "w") as f:
            for d in para_compile.get("details", []):
                f.write(json.dumps(d) + "\n")

    prog_tasks = suites["program_completion"]
    if prog_tasks:
        print(
            f"\n--- Program completion ({len(prog_tasks)} tasks, max_tokens=1024) ---",
            flush=True,
        )
        prog_gen = batch_generate(llm, prog_tasks, max_new_tokens=1024)
        prog_quality = eval_generation_quality(
            prog_tasks, prog_gen, "program_completion"
        )
        all_results["program_completion"] = prog_quality
        prog_compile = eval_compilation(
            [{"id": t["id"], "prompt": t["prompt"]} for t in prog_tasks],
            prog_gen,
            [t["reference_text"] for t in prog_tasks],
        )
        all_results["program_compilation"] = {
            k: v for k, v in prog_compile.items() if k != "details"
        }
        with open(os.path.join(OUT, "program_compilation_details.jsonl"), "w") as f:
            for d in prog_compile.get("details", []):
                f.write(json.dumps(d) + "\n")

    strv_tasks = suites["structural_validation"]
    if strv_tasks:
        print(
            f"\n--- Structural validation ({len(strv_tasks)} tasks, max_tokens=1024) ---",
            flush=True,
        )
        strv_gen = batch_generate(llm, strv_tasks, max_new_tokens=1024)
        strv_results = eval_structural(strv_tasks, strv_gen)
        all_results["structural_validation"] = strv_results["summary"]
        strv_compile = eval_compilation(
            [{"id": t["id"], "prompt": t["prompt"]} for t in strv_tasks],
            strv_gen,
            [t["reference_text"] for t in strv_tasks],
        )
        all_results["structural_compilation"] = {
            k: v for k, v in strv_compile.items() if k != "details"
        }
        with open(os.path.join(OUT, "structural_compilation_details.jsonl"), "w") as f:
            for d in strv_compile.get("details", []):
                f.write(json.dumps(d) + "\n")

    cpyb_tasks = suites["copybook_usage"]
    print(
        f"\n--- Copybook usage ({len(cpyb_tasks)} tasks, max_tokens={MAX_NEW_TOKENS}) ---",
        flush=True,
    )
    cpyb_gen = batch_generate(llm, cpyb_tasks, max_new_tokens=MAX_NEW_TOKENS)
    all_results["copybook_usage"] = eval_generation_quality(
        cpyb_tasks, cpyb_gen, "copybook_usage"
    )

    summary_path = os.path.join(OUT, "eval-summary.json")
    with open(summary_path, "w") as f:
        json.dump(all_results, f, indent=2)
    print(f"\n=== Evaluation complete. Results: {summary_path} ===", flush=True)

    print("\n=== Results Summary ===", flush=True)
    if "perplexity" in all_results:
        print(
            f"  COBOL Perplexity: {all_results['perplexity']['aggregate_perplexity']}",
            flush=True,
        )
    if "paragraph_completion" in all_results:
        print(
            f"  Paragraph BLEU: {all_results['paragraph_completion']['avg_bleu']}",
            flush=True,
        )
        print(
            f"  Paragraph Edit Dist: {all_results['paragraph_completion']['avg_edit_distance']}",
            flush=True,
        )
    if "paragraph_compilation" in all_results:
        print(
            f"  Paragraph Compile Rate: {all_results['paragraph_compilation']['compilation_rate_pct']}%",
            flush=True,
        )
    if "program_completion" in all_results:
        print(
            f"  Program BLEU: {all_results['program_completion']['avg_bleu']}",
            flush=True,
        )
    if "program_compilation" in all_results:
        print(
            f"  Program Compile Rate: {all_results['program_compilation']['compilation_rate_pct']}%",
            flush=True,
        )
    if "structural_validation" in all_results:
        sv = all_results["structural_validation"]
        print(
            f"  Division Structure: {sv['division_structure_valid_pct']}%", flush=True
        )
        print(f"  Column Format: {sv['column_format_valid_pct']}%", flush=True)
        print(f"  PIC Accuracy: {sv['pic_accuracy_pct']}%", flush=True)
        print(f"  PERFORM Ref Accuracy: {sv['perform_ref_accuracy_pct']}%", flush=True)
    print(f"  Copybook BLEU: {all_results['copybook_usage']['avg_bleu']}", flush=True)


if __name__ == "__main__":
    main()
