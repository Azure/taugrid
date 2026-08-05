#!/usr/bin/env python3
"""Run lightweight accessibility checks against generated Tau documentation."""

from __future__ import annotations

import re
import sys
import math
from html.parser import HTMLParser
from pathlib import Path


class DocumentAudit(HTMLParser):
    def __init__(self, path: Path) -> None:
        super().__init__()
        self.path = path
        self.failures: list[str] = []
        self.heading_levels: list[int] = []
        self.ids: set[str] = set()
        self.main_count = 0
        self.html_has_lang = False

    def handle_starttag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        values = dict(attrs)
        if tag == "html":
            self.html_has_lang = bool(values.get("lang"))
        elif tag == "main":
            self.main_count += 1
        elif tag == "img" and "alt" not in values:
            self.failures.append("image is missing an alt attribute")
        elif re.fullmatch(r"h[1-6]", tag):
            self.heading_levels.append(int(tag[1]))

        element_id = values.get("id")
        if element_id:
            if element_id in self.ids:
                self.failures.append(f"duplicate id {element_id!r}")
            self.ids.add(element_id)

    def finish(self) -> list[str]:
        if not self.html_has_lang:
            self.failures.append("html element is missing a language")
        if self.main_count != 1:
            self.failures.append(f"expected one main landmark, found {self.main_count}")
        if not self.heading_levels or self.heading_levels[0] != 1:
            self.failures.append("document does not begin its heading outline with h1")
        for previous, current in zip(self.heading_levels, self.heading_levels[1:]):
            if current > previous + 1:
                self.failures.append(
                    f"heading outline jumps from h{previous} to h{current}"
                )
        return self.failures


def relative_luminance(color: str) -> float:
    if color.startswith("#"):
        channels = [
            int(color[index : index + 2], 16) / 255
            for index in (1, 3, 5)
        ]
        linear = [
            channel / 12.92
            if channel <= 0.04045
            else ((channel + 0.055) / 1.055) ** 2.4
            for channel in channels
        ]
    else:
        match = re.fullmatch(
            r"oklch\((?P<lightness>[\d.]+)%\s+"
            r"(?P<chroma>[\d.]+)\s+(?P<hue>[\d.]+)\)",
            color,
        )
        if not match:
            raise ValueError(f"unsupported color {color!r}")
        lightness = float(match.group("lightness")) / 100
        chroma = float(match.group("chroma"))
        hue = math.radians(float(match.group("hue")))
        a = chroma * math.cos(hue)
        b = chroma * math.sin(hue)
        l_root = lightness + 0.3963377774 * a + 0.2158037573 * b
        m_root = lightness - 0.1055613458 * a - 0.0638541728 * b
        s_root = lightness - 0.0894841775 * a - 1.291485548 * b
        l_value, m_value, s_value = l_root**3, m_root**3, s_root**3
        linear = [
            4.0767416621 * l_value
            - 3.3077115913 * m_value
            + 0.2309699292 * s_value,
            -1.2684380046 * l_value
            + 2.6097574011 * m_value
            - 0.3413193965 * s_value,
            -0.0041960863 * l_value
            - 0.7034186147 * m_value
            + 1.707614701 * s_value,
        ]
        linear = [min(1, max(0, channel)) for channel in linear]
    return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2]


def contrast_ratio(foreground: str, background: str) -> float:
    lighter, darker = sorted(
        (relative_luminance(foreground), relative_luminance(background)),
        reverse=True,
    )
    return (lighter + 0.05) / (darker + 0.05)


def parse_theme_tokens(source: str, selector: str) -> dict[str, str]:
    match = re.search(rf"{re.escape(selector)}\s*\{{(?P<body>.*?)\n\}}", source, re.S)
    if not match:
        raise ValueError(f"missing theme block {selector}")
    return dict(
        re.findall(
            r"--(cp-[\w-]+):\s*"
            r"(#[0-9a-fA-F]{6}|oklch\([\d.]+%\s+[\d.]+\s+[\d.]+\));",
            match.group("body"),
        )
    )


def check_theme(source_path: Path) -> list[str]:
    source = source_path.read_text(encoding="utf-8")
    failures: list[str] = []
    for marker in (":focus-visible", "@media (prefers-reduced-motion: reduce)"):
        if marker not in source:
            failures.append(f"styles are missing {marker}")

    themes = {
        "light": parse_theme_tokens(source, ":root"),
        "dark": parse_theme_tokens(source, 'html[data-theme="dark"]'),
    }
    for name, tokens in themes.items():
        pairs = (
            ("cp-text", "cp-bg", 4.5),
            ("cp-text-soft", "cp-bg", 4.5),
            ("cp-link", "cp-bg", 4.5),
            ("cp-accent", "cp-bg", 3),
            ("cp-accent-fg", "cp-accent", 4.5),
            ("cp-hero-text", "cp-hero", 4.5),
            ("cp-hero-soft", "cp-hero", 3),
            ("cp-code-inline-text", "cp-code-inline-bg", 4.5),
        )
        for foreground, background, minimum in pairs:
            ratio = contrast_ratio(tokens[foreground], tokens[background])
            if ratio < minimum:
                failures.append(
                    f"{name} {foreground} on {background} has {ratio:.2f}:1 contrast"
                )
    code_tokens = themes["light"]
    for foreground in (
        "cp-code-text",
        "cp-code-comment",
        "cp-code-keyword",
        "cp-code-name",
        "cp-code-string",
        "cp-code-number",
        "cp-code-error",
    ):
        ratio = contrast_ratio(code_tokens[foreground], code_tokens["cp-code-bg"])
        if ratio < 4.5:
            failures.append(
                f"code {foreground} on cp-code-bg has {ratio:.2f}:1 contrast"
            )
    return failures


def main() -> int:
    public_dir = Path(sys.argv[1] if len(sys.argv) > 1 else "public")
    source_path = Path(
        sys.argv[2]
        if len(sys.argv) > 2
        else "assets/scss/_styles_project.scss"
    )
    failures: list[str] = []
    documents = sorted(public_dir.rglob("*.html"))
    for path in documents:
        audit = DocumentAudit(path)
        audit.feed(path.read_text(encoding="utf-8"))
        failures.extend(f"{path}: {failure}" for failure in audit.finish())
    failures.extend(f"{source_path}: {failure}" for failure in check_theme(source_path))

    if failures:
        for failure in failures:
            print(f"Accessibility error: {failure}", file=sys.stderr)
        return 1

    print(f"Checked accessibility structure for {len(documents)} HTML documents")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
