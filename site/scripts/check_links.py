#!/usr/bin/env python3

from __future__ import annotations

import argparse
import html.parser
import pathlib
import sys
import urllib.parse


class DocumentParser(html.parser.HTMLParser):
    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.ids: set[str] = set()
        self.targets: list[str] = []

    def handle_starttag(
        self, tag: str, attrs: list[tuple[str, str | None]]
    ) -> None:
        values = dict(attrs)
        if identifier := values.get("id"):
            self.ids.add(identifier)
        attribute = "href" if tag in {"a", "link"} else "src"
        if tag in {"a", "link", "img", "script", "source"}:
            if target := values.get(attribute):
                self.targets.append(target)


def document_url(relative_path: pathlib.Path, base_path: str) -> str:
    path = relative_path.as_posix()
    if path == "index.html":
        return f"{base_path}/"
    if path.endswith("/index.html"):
        path = path[: -len("index.html")]
    return f"{base_path}/{path}".replace("//", "/")


def target_file(
    root: pathlib.Path, target_path: str, base_path: str
) -> pathlib.Path:
    decoded = urllib.parse.unquote(target_path)
    for prefix in (f"{base_path}/", base_path):
        if prefix != "/" and decoded.startswith(prefix):
            decoded = decoded[len(prefix) :]
            break
    decoded = decoded.lstrip("/")
    candidate = root / decoded
    if decoded == "" or decoded.endswith("/"):
        candidate /= "index.html"
    elif candidate.suffix == "":
        candidate /= "index.html"
    return candidate


def main() -> int:
    parser = argparse.ArgumentParser(description="Check generated Hugo links")
    parser.add_argument("public_dir", type=pathlib.Path)
    parser.add_argument("--base-path", default="/taugrid")
    args = parser.parse_args()

    root = args.public_dir.resolve()
    documents: dict[pathlib.Path, DocumentParser] = {}
    for path in root.rglob("*.html"):
        parsed = DocumentParser()
        parsed.feed(path.read_text(encoding="utf-8"))
        documents[path.resolve()] = parsed

    errors: list[str] = []
    for source, parsed in documents.items():
        relative = source.relative_to(root)
        source_url = document_url(relative, args.base_path)
        for raw_target in parsed.targets:
            if raw_target.startswith(("data:", "mailto:", "tel:", "javascript:")):
                continue
            resolved = urllib.parse.urljoin(source_url, raw_target)
            target = urllib.parse.urlparse(resolved)
            if target.scheme or target.netloc:
                continue
            decoded_path = urllib.parse.unquote(target.path)
            if (
                args.base_path != "/"
                and decoded_path.startswith("/")
                and decoded_path != args.base_path
                and not decoded_path.startswith(f"{args.base_path}/")
            ):
                errors.append(
                    f"{relative}: root-relative target is outside "
                    f"{args.base_path}: {raw_target}"
                )
                continue
            destination = target_file(root, target.path, args.base_path).resolve()
            if not destination.is_relative_to(root):
                errors.append(f"{relative}: target escapes public directory: {raw_target}")
                continue
            if not destination.exists():
                errors.append(f"{relative}: missing target: {raw_target}")
                continue
            if target.fragment and destination.suffix == ".html":
                destination_document = documents.get(destination)
                if destination_document and target.fragment not in destination_document.ids:
                    errors.append(
                        f"{relative}: missing fragment #{target.fragment} in "
                        f"{destination.relative_to(root)}"
                    )

    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 1
    print(f"Checked {len(documents)} generated HTML documents")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
