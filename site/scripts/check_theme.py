#!/usr/bin/env python3
"""Validate the generated site's early theme initialization contract."""

from __future__ import annotations

import sys
from html.parser import HTMLParser
from pathlib import Path


class ScriptCollector(HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.scripts: list[tuple[dict[str, str | None], str]] = []
        self._attrs: dict[str, str | None] | None = None
        self._body: list[str] = []

    def handle_starttag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        if tag == "script" and self._attrs is None:
            self._attrs = dict(attrs)
            self._body = []

    def handle_data(self, data: str) -> None:
        if self._attrs is not None:
            self._body.append(data)

    def handle_endtag(self, tag: str) -> None:
        if tag == "script" and self._attrs is not None:
            self.scripts.append((self._attrs, "".join(self._body)))
            self._attrs = None
            self._body = []


def main() -> int:
    public_dir = Path(sys.argv[1] if len(sys.argv) > 1 else "public")
    index = public_dir / "index.html"
    if not index.is_file():
        print(f"Missing generated homepage: {index}", file=sys.stderr)
        return 1

    parser = ScriptCollector()
    parser.feed(index.read_text(encoding="utf-8"))
    if not parser.scripts:
        print("Generated homepage has no scripts", file=sys.stderr)
        return 1

    attrs, body = parser.scripts[0]
    failures: list[str] = []
    if attrs.get("src"):
        failures.append("the first script is external")
    for marker in (
        "scoutTheme",
        "prefers-color-scheme: dark",
        'setAttribute("data-theme"',
        'setAttribute("data-bs-theme"',
        "MutationObserver",
    ):
        if marker not in body:
            failures.append(f"the first script is missing {marker!r}")

    css = "\n".join(
        path.read_text(encoding="utf-8")
        for path in (public_dir / "scss").glob("*.css")
    )
    for token in ("--cp-bg:", "--cp-accent:", "--cp-text:"):
        if token not in css:
            failures.append(f"generated CSS is missing {token}")

    if failures:
        for failure in failures:
            print(f"Theme contract error: {failure}", file=sys.stderr)
        return 1

    print("Validated early theme initialization and generated design tokens")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
