#!/usr/bin/env python3
"""Compares evaluation result files side by side (one column per file):

    evals/compare.py evals/results/*-claude.json evals/results/*-vllm.json > evals/results/COMPARISON.md

Each cell shows passed/runs for a case; the summary rows show the pass rate, mean seconds
and mean tool calls over all runs of the file.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path


def mean(values: list[float]) -> str:
    return f"{sum(values) / len(values):.1f}" if values else "-"


def main(paths: list[str]) -> int:
    if not paths:
        print(__doc__, file=sys.stderr)
        return 2
    runs = [json.loads(Path(p).read_text()) for p in paths]
    cases = sorted({r["case"] for run in runs for r in run["results"]})
    head = [f"{run['provider']} ({run['model']})<br>{run['started'][:16]}" for run in runs]
    lines = ["| Case | " + " | ".join(head) + " |", "|---|" + "---|" * len(runs)]
    for case in cases:
        cells = []
        for run in runs:
            rs = [r for r in run["results"] if r["case"] == case]
            cells.append(f"{sum(r['passed'] for r in rs)}/{len(rs)}" if rs else "-")
        lines.append(f"| {case} | " + " | ".join(cells) + " |")
    summary = {
        "Pass rate": lambda rs: f"{100 * sum(r['passed'] for r in rs) / len(rs):.0f}% ({sum(r['passed'] for r in rs)}/{len(rs)})",
        "Root cause found": lambda rs: f"{100 * sum(r['checks'].get('root_cause', False) for r in rs) / len(rs):.0f}%",
        "Right playbook": lambda rs: f"{100 * sum(r['checks'].get('playbook', True) for r in rs) / len(rs):.0f}%",
        "Errors (no diagnosis)": lambda rs: str(sum(bool(r["error"]) or not r["checks"].get("diagnosed") for r in rs)),
        "Mean seconds": lambda rs: mean([r["seconds"] for r in rs if r.get("seconds") is not None]),
        "Mean tool calls": lambda rs: mean([r["tool_calls"] for r in rs if r.get("tool_calls") is not None]),
        "Fallbacks": lambda rs: None,
    }
    for name, fn in summary.items():
        if name == "Fallbacks":
            cells = [", ".join(run.get("fallbacks") or []) or "none" for run in runs]
        else:
            cells = [fn(run["results"]) if run["results"] else "-" for run in runs]
        lines.append(f"| **{name}** | " + " | ".join(cells) + " |")
    print("\n".join(lines))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
