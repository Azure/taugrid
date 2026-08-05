#!/usr/bin/env python3

from __future__ import annotations

import argparse
import datetime
import pathlib
import re
import sys
import tomllib
import urllib.parse


ALLOWED_MATURITY = {"shipped", "experimental", "implementing", "future"}
MATURITY_PATTERN = re.compile(
    r'\{\{<\s*maturity\s+status="(?P<status>[^"]+)"\s+'
    r'reviewed="(?P<reviewed>\d{4}-\d{2}-\d{2})"\s*>\}\}'
)
WIKI_PREFIX = (
    "https://github.com/Azure/taugrid/wiki/"
)


def main() -> int:
    parser = argparse.ArgumentParser(description="Check site/wiki ownership metadata")
    parser.add_argument(
        "--manifest",
        type=pathlib.Path,
        default=pathlib.Path("data/wiki-parity.toml"),
    )
    parser.add_argument("--max-age-days", type=int, default=90)
    args = parser.parse_args()

    site_root = args.manifest.resolve().parent.parent
    manifest = tomllib.loads(args.manifest.read_text(encoding="utf-8"))
    pairs = manifest.get("pairs", [])
    site_only = manifest.get("site_only", [])
    errors: list[str] = []
    seen: set[str] = set()
    today = datetime.date.today()

    for pair in pairs:
        identifier = pair.get("id", "")
        if not identifier or identifier in seen:
            errors.append(f"invalid or duplicate pair id: {identifier!r}")
        seen.add(identifier)

        site_path = site_root / pair.get("site", "")
        if not site_path.is_file():
            errors.append(f"{identifier}: missing site page {site_path}")
            site_metadata = None
        else:
            site_metadata = MATURITY_PATTERN.search(
                site_path.read_text(encoding="utf-8")
            )
            if not site_metadata:
                errors.append(f"{identifier}: site page is missing maturity metadata")

        wiki = pair.get("wiki", "")
        parsed = urllib.parse.urlparse(wiki)
        if not wiki.startswith(WIKI_PREFIX) or parsed.scheme != "https":
            errors.append(f"{identifier}: invalid wiki URL {wiki!r}")

        if not pair.get("owner"):
            errors.append(f"{identifier}: missing owner")

        maturity = pair.get("maturity")
        if maturity not in ALLOWED_MATURITY:
            errors.append(f"{identifier}: unsupported maturity {maturity!r}")
        elif site_metadata and site_metadata.group("status") != maturity:
            errors.append(
                f"{identifier}: manifest maturity {maturity!r} does not match "
                f"site maturity {site_metadata.group('status')!r}"
            )

        try:
            reviewed = datetime.date.fromisoformat(pair.get("reviewed", ""))
        except ValueError:
            errors.append(f"{identifier}: invalid review date")
            continue
        age = (today - reviewed).days
        if reviewed > today:
            errors.append(f"{identifier}: review date {reviewed} is in the future")
        elif age > args.max_age_days:
            errors.append(
                f"{identifier}: site/wiki review is {age} days old "
                f"(limit {args.max_age_days})"
            )
        if site_metadata:
            site_reviewed = datetime.date.fromisoformat(
                site_metadata.group("reviewed")
            )
            if site_reviewed > reviewed:
                errors.append(
                    f"{identifier}: site review {site_reviewed} is newer than "
                    f"site/wiki parity review {reviewed}"
                )

    for entry in site_only:
        identifier = entry.get("id", "")
        if not identifier or identifier in seen:
            errors.append(f"invalid or duplicate site-only id: {identifier!r}")
        seen.add(identifier)

        site_path = site_root / entry.get("site", "")
        if not site_path.is_file():
            errors.append(f"{identifier}: missing site-only page {site_path}")
        if not entry.get("owner"):
            errors.append(f"{identifier}: missing owner")
        if not entry.get("reason"):
            errors.append(f"{identifier}: missing site-only rationale")

        try:
            reviewed = datetime.date.fromisoformat(entry.get("reviewed", ""))
        except ValueError:
            errors.append(f"{identifier}: invalid review date")
            continue
        age = (today - reviewed).days
        if reviewed > today:
            errors.append(f"{identifier}: review date {reviewed} is in the future")
        elif age > args.max_age_days:
            errors.append(
                f"{identifier}: site-only rationale is {age} days old "
                f"(limit {args.max_age_days})"
            )

    if not pairs:
        errors.append("parity manifest contains no pairs")

    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 1
    print(
        f"Checked {len(pairs)} separately maintained site/wiki pairs "
        f"and {len(site_only)} reviewed site-only groups"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
