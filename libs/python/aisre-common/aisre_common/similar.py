"""Similar past incidents: one full-text query shared by the controller and the web console.

The controller owns the `incidents` table and its `incidents_fts` index; the expression in
`DOCUMENT` must stay identical to the indexed one, or Postgres falls back to a full scan.
"""

from __future__ import annotations

from typing import Any, Iterable

# Alerts with this label come from the evaluation runner; their incidents are never hints.
EVAL_LABEL = "aisre_eval"

DOCUMENT = "to_tsvector('simple', title || ' ' || coalesce(root_cause, '') || ' ' || alerts::text)"

# $1 incident id to exclude, $2 tsquery from `tsquery()`, $3 limit.
SQL = f"""select id, title, status, root_cause, confidence, playbook, decision->>'decision' as decision, created_at,
       ts_rank({DOCUMENT}, to_tsquery('simple', $2)) as rank
from incidents
where id <> $1 and root_cause is not null and coalesce(confidence, 0) >= 0.5
  and not jsonb_path_exists(alerts, '$[*].labels.{EVAL_LABEL}')
  and {DOCUMENT} @@ to_tsquery('simple', $2)
order by rank desc, created_at desc limit $3"""


def terms(title: str, alerts: Iterable[dict[str, Any]], target: dict[str, Any] | None) -> list[str]:
    """Words that identify an incident: title, alert names, workload.

    Namespace and kind are left out: they are shared by unrelated incidents and made
    every incident in a namespace look similar.
    """
    words = {title or ""}
    for a in alerts or []:
        words.add(a.get("alertname") or "")
    if target and target.get("workload"):
        words.add(str(target["workload"]))
    out = []
    for w in " ".join(words).replace("-", " ").split():
        w = "".join(c for c in w if c.isalnum() or c in "_.")
        if len(w) > 2 and w not in out:
            out.append(w)
    return out


def tsquery(title: str, alerts: Iterable[dict[str, Any]], target: dict[str, Any] | None) -> str:
    """OR-query over `terms()`; empty when there is nothing to search for."""
    return " | ".join(f"'{w}'" for w in terms(title, alerts, target))
