# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import re
import subprocess
from pathlib import Path

MICROSOFT_COPYRIGHT = "Copyright (c) Microsoft Corporation."
MIT_LICENSE = "Licensed under the MIT License."
ADD_COMMAND = "./scripts/add-license-headers.py"
EXCLUSIONS_FILE = "scripts/license-header-exclusions.txt"

SLASH_HEADER = (
    f"// {MICROSOFT_COPYRIGHT}",
    f"// {MIT_LICENSE}",
)
HASH_HEADER = (
    f"# {MICROSOFT_COPYRIGHT}",
    f"# {MIT_LICENSE}",
)
DOUBLE_COLON_HEADER = (
    f":: {MICROSOFT_COPYRIGHT}",
    f":: {MIT_LICENSE}",
)
DOUBLE_SEMICOLON_HEADER = (
    f";; {MICROSOFT_COPYRIGHT}",
    f";; {MIT_LICENSE}",
)
DOUBLE_DASH_HEADER = (
    f"-- {MICROSOFT_COPYRIGHT}",
    f"-- {MIT_LICENSE}",
)
PERCENT_HEADER = (
    f"% {MICROSOFT_COPYRIGHT}",
    f"% {MIT_LICENSE}",
)
C_BLOCK_HEADER = (
    "/*",
    f" * {MICROSOFT_COPYRIGHT}",
    f" * {MIT_LICENSE}",
    " */",
)
OCAML_HEADER = (
    f"(* {MICROSOFT_COPYRIGHT} *)",
    f"(* {MIT_LICENSE} *)",
)
HASKELL_HEADER = (
    "{-",
    "  Copyright : (c) Microsoft Corporation.",
    "  License : MIT",
    "-}",
)
HTML_HEADER = (
    f"<!-- {MICROSOFT_COPYRIGHT}",
    f"     {MIT_LICENSE} -->",
)

EXTENSION_HEADERS = {
    ".bat": DOUBLE_COLON_HEADER,
    ".bicep": SLASH_HEADER,
    ".c": C_BLOCK_HEADER,
    ".cc": SLASH_HEADER,
    ".cmd": DOUBLE_COLON_HEADER,
    ".coffee": HASH_HEADER,
    ".cpp": SLASH_HEADER,
    ".cs": SLASH_HEADER,
    ".cxx": SLASH_HEADER,
    ".dart": SLASH_HEADER,
    ".el": DOUBLE_SEMICOLON_HEADER,
    ".fs": SLASH_HEADER,
    ".fsi": SLASH_HEADER,
    ".fsx": SLASH_HEADER,
    ".glsl": SLASH_HEADER,
    ".go": SLASH_HEADER,
    ".gradle": SLASH_HEADER,
    ".groovy": SLASH_HEADER,
    ".h": C_BLOCK_HEADER,
    ".hh": SLASH_HEADER,
    ".hpp": SLASH_HEADER,
    ".hs": HASKELL_HEADER,
    ".html": HTML_HEADER,
    ".hxx": SLASH_HEADER,
    ".java": SLASH_HEADER,
    ".jl": HASH_HEADER,
    ".js": SLASH_HEADER,
    ".jsx": SLASH_HEADER,
    ".kt": C_BLOCK_HEADER,
    ".kts": C_BLOCK_HEADER,
    ".lhs": HASKELL_HEADER,
    ".lua": DOUBLE_DASH_HEADER,
    ".m": SLASH_HEADER,
    ".ml": OCAML_HEADER,
    ".mli": OCAML_HEADER,
    ".mjs": SLASH_HEADER,
    ".mm": SLASH_HEADER,
    ".php": SLASH_HEADER,
    ".pl": HASH_HEADER,
    ".pm": HASH_HEADER,
    ".ps1": HASH_HEADER,
    ".py": HASH_HEADER,
    ".r": HASH_HEADER,
    ".rb": HASH_HEADER,
    ".rs": SLASH_HEADER,
    ".scala": SLASH_HEADER,
    ".sh": HASH_HEADER,
    ".sql": DOUBLE_DASH_HEADER,
    ".tex": PERCENT_HEADER,
    ".ts": SLASH_HEADER,
    ".tsx": SLASH_HEADER,
}

COPYRIGHT_PATTERN = re.compile(r"\bcopyright\b", re.IGNORECASE)
MIT_PATTERN = re.compile(
    r"licensed\s+under\s+the\s+mit\s+license|license\s*:\s*mit\b",
    re.IGNORECASE,
)
PYTHON_ENCODING_PATTERN = re.compile(r"coding[:=]\s*[-\w.]+")
UTF8_BOM = b"\xef\xbb\xbf"


def repository_root() -> Path:
    result = subprocess.run(
        ["git", "rev-parse", "--show-toplevel"],
        check=True,
        stdout=subprocess.PIPE,
        text=True,
    )
    return Path(result.stdout.strip())


def tracked_and_untracked_files(root: Path) -> list[str]:
    result = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=root,
        check=True,
        stdout=subprocess.PIPE,
    )
    return sorted(path.decode("utf-8") for path in result.stdout.split(b"\0") if path)


def load_exclusions(root: Path) -> tuple[set[str], tuple[str, ...]]:
    exact: set[str] = set()
    prefixes: list[str] = []
    exclusion_path = root / EXCLUSIONS_FILE
    for raw_line in exclusion_path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if line.endswith("/"):
            prefixes.append(line)
        else:
            exact.add(line)
    return exact, tuple(prefixes)


def is_excluded(path: str, exact: set[str], prefixes: tuple[str, ...]) -> bool:
    return path in exact or path.startswith(prefixes)


def source_name(path: str) -> str:
    name = Path(path).name
    while name.lower().endswith(".tmpl"):
        name = name[:-5]
    return name


def header_for(path: str) -> tuple[str, ...] | None:
    name = source_name(path)
    lower_name = name.lower()
    if lower_name == "cmakelists.txt" or lower_name.endswith(".cmake"):
        return HASH_HEADER
    if lower_name == "makefile" or lower_name.startswith("makefile."):
        return HASH_HEADER
    if lower_name == "dockerfile" or lower_name.startswith("dockerfile."):
        return HASH_HEADER
    return EXTENSION_HEADERS.get(Path(lower_name).suffix)


def source_files(root: Path) -> list[tuple[str, tuple[str, ...]]]:
    exact, prefixes = load_exclusions(root)
    files: list[tuple[str, tuple[str, ...]]] = []
    for relative_path in tracked_and_untracked_files(root):
        if is_excluded(relative_path, exact, prefixes):
            continue
        header = header_for(relative_path)
        full_path = root / relative_path
        if header is not None and full_path.is_file() and not full_path.is_symlink():
            files.append((relative_path, header))
    return files


def leading_text(text: str) -> str:
    return "\n".join(text.splitlines()[:40])


def has_complete_header(text: str) -> bool:
    prefix = leading_text(text)
    return bool(COPYRIGHT_PATTERN.search(prefix) and MIT_PATTERN.search(prefix))


def has_copyright_notice(text: str) -> bool:
    return bool(COPYRIGHT_PATTERN.search(leading_text(text)))


def insertion_index(path: str, lines: list[str]) -> int:
    if not lines:
        return 0

    first = lines[0].strip()
    if Path(source_name(path)).suffix.lower() == ".html":
        if first in {"+++", "---"}:
            for index, line in enumerate(lines[1:], start=1):
                if line.strip() == first:
                    return index + 1
        if first.startswith("{{") and "define " in first:
            return 1

    index = 0
    if lines[0].startswith("#!"):
        index = 1
    if Path(source_name(path)).suffix.lower() == ".php" and first.startswith("<?php"):
        index = 1
    if (
        Path(source_name(path)).suffix.lower() == ".py"
        and index < len(lines)
        and PYTHON_ENCODING_PATTERN.search(lines[index])
    ):
        index += 1
    return index


def add_header(path: str, text: str, header: tuple[str, ...]) -> str:
    newline = "\r\n" if "\r\n" in text else "\n"
    lines = text.splitlines(keepends=True)
    index = insertion_index(path, lines)
    before = "".join(lines[:index])
    after = "".join(lines[index:])
    if before and not before.endswith(("\n", "\r")):
        before += newline
    separator = newline * 2 if after else newline
    return before + newline.join(header) + separator + after


def read_source(path: Path) -> tuple[bool, str]:
    data = path.read_bytes()
    has_bom = data.startswith(UTF8_BOM)
    if has_bom:
        data = data[len(UTF8_BOM) :]
    return has_bom, data.decode("utf-8")


def write_source(path: Path, has_bom: bool, text: str) -> None:
    data = text.encode("utf-8")
    if has_bom:
        data = UTF8_BOM + data
    path.write_bytes(data)


def check_headers() -> int:
    root = repository_root()
    files = source_files(root)
    missing: list[str] = []
    for relative_path, _ in files:
        _, text = read_source(root / relative_path)
        if not has_complete_header(text):
            missing.append(relative_path)

    if not missing:
        print(f"License header check passed for {len(files)} source files.")
        return 0

    print("The following source files are missing a copyright and MIT license header:")
    for path in missing:
        print(f"  {path}")
    print(f"\nMicrosoft contributors: run `{ADD_COMMAND}` and commit the changes.")
    print(
        "Third-party contributors may add their own copyright and MIT license "
        "header; do not add a Microsoft copyright notice to third-party code."
    )
    return 1


def add_headers() -> int:
    root = repository_root()
    print(
        "Adding Microsoft license headers to files without an existing copyright "
        f"notice; known third-party sources are listed in {EXCLUSIONS_FILE}."
    )
    updated: list[str] = []
    manual_review: list[str] = []
    files = source_files(root)
    for relative_path, header in files:
        full_path = root / relative_path
        has_bom, text = read_source(full_path)
        if has_complete_header(text):
            continue
        if has_copyright_notice(text):
            manual_review.append(relative_path)
            continue
        write_source(full_path, has_bom, add_header(relative_path, text, header))
        updated.append(relative_path)

    for path in updated:
        print(f"added license header: {path}")
    print(f"Added license headers to {len(updated)} of {len(files)} source files.")

    if manual_review:
        print("\nExisting copyright notices were preserved and need manual review:")
        for path in manual_review:
            print(f"  {path}")
        return 1
    return 0
