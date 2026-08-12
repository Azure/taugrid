#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.


from __future__ import annotations

import argparse
import datetime
import pathlib
import re
import sys


MATURITY_PATTERN = re.compile(
    r'\{\{<\s*maturity\s+status="(?P<status>[^"]+)"\s+'
    r'reviewed="(?P<reviewed>\d{4}-\d{2}-\d{2})"\s*>\}\}'
)
ALLOWED_STATUSES = {
    "alpha",
    "beta",
    "ga",
    "deprecated",
    "planned",
    # Compatibility aliases for content that has not migrated yet.
    "shipped",
    "experimental",
    "implementing",
    "future",
}


def main() -> int:
    parser = argparse.ArgumentParser(description="Validate Tau documentation metadata")
    parser.add_argument("content_dir", type=pathlib.Path)
    parser.add_argument("--max-age-days", type=int, default=120)
    args = parser.parse_args()

    today = datetime.date.today()
    errors: list[str] = []
    pages = sorted(args.content_dir.rglob("*.md"))
    capability_pages = 0
    for page in pages:
        text = page.read_text(encoding="utf-8")
        match = MATURITY_PATTERN.search(text)
        if page.name == "_index.md":
            if match:
                errors.append(
                    f"{page}: section indexes must not declare feature maturity"
                )
            continue
        if not match:
            continue
        capability_pages += 1
        status = match.group("status")
        if status not in ALLOWED_STATUSES:
            errors.append(f"{page}: unsupported maturity status {status!r}")
        reviewed = datetime.date.fromisoformat(match.group("reviewed"))
        if reviewed > today:
            errors.append(f"{page}: review date {reviewed} is in the future")
        elif (today - reviewed).days > args.max_age_days:
            errors.append(
                f"{page}: review is {(today - reviewed).days} days old "
                f"(limit {args.max_age_days})"
            )

    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 1
    print(f"Checked maturity metadata for {capability_pages} capability pages")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
