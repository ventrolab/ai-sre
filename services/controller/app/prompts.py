"""Prompts for HolmesGPT and parsing of its structured answer."""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any

from .db import Incident

# Answers go to chat and the web console as plain Markdown.
OUTPUT_STYLE = """Output style: plain text and simple Markdown (bold, lists, code). Never use
emoji or other pictographic symbols, including check marks and crosses for step
status; write "done", "failed" or "skipped" instead.
"""

CHAT_REPORT_RULES = """"chat_report" is read on a phone by the on-call engineer. Markdown, at most
8 short lines, no headings, no step-by-step narrative of the investigation:
- line 1: what is broken (object and symptom);
- **Root cause:** one or two sentences;
- **Evidence:** up to three short bullets with the decisive facts (log line, exit
  code, event, metric value);
- **Fix:** one sentence; say if a restart or rollback will not help.
If the root cause is unknown, say so and list what was ruled out.
"""

SYSTEM_PROMPT = """You are the investigation engine of an automated SRE system.
You run unattended: nobody will answer questions, so never ask the user
anything and never stop with suggestions for further steps. Investigate with
the available read-only tools until you can name the root cause, then answer.

How to investigate:
- Do not guess when a tool can answer.
- For a pod: describe it (events, exit codes, restart reasons), then read its
  logs WITHOUT a filter, including the previous container instance; only then
  narrow down. Log filters are case-sensitive, so prefer no filter over a
  guessed one.
- Check the owning workload, recent changes, and related metrics if relevant.
- Never suggest commands that change state; remediation is chosen only from
  the playbook list given in the question.

Answer in two parts:
1. A concise human-readable analysis: what is wrong, the evidence you found
   (metrics, logs, events), and the root cause.
2. On the last lines, a JSON block in a ```json fence with exactly these keys:
   {"summary": "<one sentence>", "root_cause": "<one or two sentences>",
    "confidence": <number 0..1>, "playbook": "<playbook name or null>",
    "playbook_reason": "<why this playbook fits, or why none does>",
    "chat_report": "<short report for the chat message, see below>"}

The full analysis is kept in the web console; the chat gets only "chat_report".
""" + CHAT_REPORT_RULES + "\n" + OUTPUT_STYLE

FINALIZE_PROMPT = """Based only on evidence from the tool results above, output
the final JSON block now, in a ```json fence. If the evidence does not show the
root cause, set "root_cause" to "unknown", "confidence" to 0 and "playbook" to
null. Do not invent causes. Keys:
{"summary": "<one sentence>", "root_cause": "<one or two sentences>",
 "confidence": <number 0..1>, "playbook": "<one of the offered playbook names or null>",
 "playbook_reason": "<why this playbook fits, or why none does>",
 "chat_report": "<short report for the chat message>"}
""" + CHAT_REPORT_RULES

CHECK_SYSTEM_PROMPT = """You are the health-check engine of an automated SRE system.
Use the read-only tools to answer the check. Finish with a ```json block:
{"healthy": <true|false>, "summary": "<one or two sentences>"}
""" + "\n" + OUTPUT_STYLE

CHAT_SYSTEM_PROMPT = """You answer questions of on-call engineers about their infrastructure,
using the read-only tools. Be concise and base the answer on tool results.
""" + "\n" + OUTPUT_STYLE


def investigation_prompt(
    incident: Incident, playbooks: list[dict[str, Any]], now: datetime | None = None, local_cluster: str = "",
    similar: list[dict[str, Any]] | None = None,
) -> str:
    t = incident.target
    now = now or datetime.now(timezone.utc)
    lines = [
        "Investigate this incident and find the root cause.",
        "",
        f"Current time: {now.strftime('%Y-%m-%dT%H:%M:%SZ')} (UTC). Alert timestamps are in the past.",
        f"Target: {t.describe()}",
    ]
    if t.host:
        lines.append(
            f"The target is the standalone host {t.host}: use the ai-sre-tools host_* tools with target={t.host}."
        )
    elif t.cluster and local_cluster and t.cluster != local_cluster:
        lines.append(
            f"The incident is in cluster {t.cluster}, not in the local cluster {local_cluster}. The built-in "
            f"Kubernetes, log and Prometheus tools only see the local cluster: use the ai-sre-tools "
            f"(k8s_list, k8s_get, k8s_events, k8s_logs, logs_query, prometheus_query) with target={t.cluster}."
        )
    if t.pod and t.workload:
        lines.append(
            f"The pod named in the alert may already be replaced: if it is not found, "
            f"list the current pods of {t.workload} in namespace {t.namespace} and investigate those."
        )
    lines.append("Alerts:")
    for alert in incident.alerts:
        lines.append(f"- {alert.get('alertname')} [{alert.get('status')}] since {alert.get('starts_at')}")
        annotations = alert.get("annotations") or {}
        for key in ("summary", "description", "message"):
            if annotations.get(key):
                lines.append(f"  {key}: {annotations[key]}")
        labels = {k: v for k, v in (alert.get("labels") or {}).items() if k != "alertname"}
        if labels:
            lines.append(f"  labels: {json.dumps(labels, ensure_ascii=False)}")
    lines.append("")
    if similar:
        lines.append("Similar past incidents (hints only; verify with tools, the cause may differ):")
        for s in similar:
            when = s["created_at"].strftime("%Y-%m-%d") if s.get("created_at") else ""
            outcome = f"; playbook {s['playbook']} ({s.get('decision') or 'proposed'})" if s.get("playbook") else ""
            lines.append(f"- {when} {s['title']} [{s['status']}]: {s['root_cause']}{outcome}")
        lines.append("")
    if playbooks:
        lines.append("Remediation playbooks that may apply (choose at most one, or null):")
        for pb in playbooks:
            lines.append(f"- {pb['name']} (risk {pb['risk']}): {pb.get('description', '')}")
        lines.append(
            "Choose a playbook only if the evidence shows it addresses the root cause. "
            "If the cause is in the application or its configuration, choose null."
        )
    else:
        lines.append("No remediation playbooks apply to this alert; set playbook to null.")
    return "\n".join(lines)


@dataclass
class Investigation:
    analysis: str
    summary: str | None = None
    root_cause: str | None = None
    confidence: float | None = None
    playbook: str | None = None
    playbook_reason: str | None = None
    chat_report: str | None = None
    structured: bool = False


_THINK = re.compile(r"<think>.*?</think>", re.S)
_FENCE = re.compile(r"```(?:json)?\s*(\{.*?\})\s*```", re.S)


def _last_json_object(text: str) -> tuple[dict[str, Any] | None, tuple[int, int] | None]:
    """Finds the last JSON object in text: fenced first, then bare braces."""
    for match in reversed(list(_FENCE.finditer(text))):
        try:
            return json.loads(match.group(1)), match.span()
        except json.JSONDecodeError:
            continue
    end = text.rfind("}")
    while end != -1:
        depth = 0
        for start in range(end, -1, -1):
            if text[start] == "}":
                depth += 1
            elif text[start] == "{":
                depth -= 1
                if depth == 0:
                    try:
                        return json.loads(text[start : end + 1]), (start, end + 1)
                    except json.JSONDecodeError:
                        break
        end = text.rfind("}", 0, end)
    return None, None


def clean(text: str) -> str:
    return _THINK.sub("", text or "").strip()


def parse_investigation(text: str, allowed_playbooks: set[str]) -> Investigation:
    text = clean(text)
    data, span = _last_json_object(text)
    if not isinstance(data, dict) or "root_cause" not in data:
        return Investigation(analysis=text)

    analysis = (text[: span[0]] + text[span[1] :]).strip() if span else text
    confidence = data.get("confidence")
    try:
        confidence = max(0.0, min(1.0, float(confidence))) if confidence is not None else None
    except (TypeError, ValueError):
        confidence = None
    playbook = data.get("playbook")
    # The model may only pick from the offered list.
    if not isinstance(playbook, str) or playbook not in allowed_playbooks:
        playbook = None
    return Investigation(
        analysis=analysis or str(data.get("summary") or ""),
        summary=data.get("summary"),
        root_cause=data.get("root_cause"),
        confidence=confidence,
        playbook=playbook,
        playbook_reason=data.get("playbook_reason"),
        chat_report=str(data.get("chat_report") or "").strip() or None,
        structured=True,
    )


def parse_check(text: str) -> tuple[bool | None, str]:
    text = clean(text)
    data, span = _last_json_object(text)
    if isinstance(data, dict) and "healthy" in data:
        return bool(data["healthy"]), str(data.get("summary") or "")
    return None, text
