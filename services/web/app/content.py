"""Read-only views of the playbook catalog and the knowledge base, and Markdown rendering."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import yaml
from markdown_it import MarkdownIt

# HTML in the source is escaped, not rendered: content may come from LLM output.
_md = MarkdownIt("commonmark", {"html": False, "linkify": False}).enable("table")


def markdown(text: str | None) -> str:
    return _md.render(text or "")


def _files(directory: str, suffix: str) -> list[Path]:
    path = Path(directory)
    if not path.is_dir():
        return []
    return sorted(p for p in path.iterdir() if p.name.endswith(suffix) and not p.name.startswith("."))


def split_frontmatter(text: str) -> tuple[dict[str, Any], str]:
    if text.startswith("---"):
        parts = text.split("---", 2)
        if len(parts) == 3:
            try:
                return yaml.safe_load(parts[1]) or {}, parts[2].lstrip()
            except yaml.YAMLError:
                pass
    return {}, text


def skills(directory: str) -> list[dict[str, Any]]:
    """Skills from the ConfigMap mount: <name>.md files with frontmatter."""
    out = []
    for path in _files(directory, ".md"):
        meta, body = split_frontmatter(path.read_text())
        out.append({
            "name": meta.get("name") or path.stem,
            "description": meta.get("description", ""),
            "body": body,
        })
    return out


def playbook_sources(directory: str) -> dict[str, str]:
    """Playbook name -> YAML source, from the catalog ConfigMap mount."""
    out = {}
    for path in _files(directory, ".yaml"):
        text = path.read_text()
        for doc in yaml.safe_load_all(text):
            if isinstance(doc, dict):
                name = (doc.get("metadata") or {}).get("name")
                if name:
                    out[name] = yaml.safe_dump(doc, sort_keys=False)
    return out
