"""Incident lifecycle: alerts -> investigation -> guard decision -> notifications."""

from __future__ import annotations

import asyncio
import logging
import time
from datetime import datetime, timezone
from typing import Any, Awaitable, Callable

from prometheus_client import Counter, Histogram

from aisre_common.models import ApprovalDecision, ChatMessage, ChatRef, Event, Notification, NotificationAction

from .clients import Guard, GuardError, Holmes, HolmesError
from .db import Incident, Repo
from .prompts import (
    CHAT_SYSTEM_PROMPT,
    CHECK_SYSTEM_PROMPT,
    FINALIZE_PROMPT,
    SYSTEM_PROMPT,
    clean,
    investigation_prompt,
    parse_check,
    parse_investigation,
)

log = logging.getLogger("controller.engine")

INCIDENTS = Counter("aisre_incidents_opened_total", "Incidents opened", ["kind"])
INVESTIGATIONS = Counter("aisre_investigations_total", "Investigations finished", ["result"])
INVESTIGATION_SECONDS = Histogram(
    "aisre_investigation_seconds", "Investigation duration", buckets=(10, 30, 60, 120, 300, 600, 900, 1800)
)
DECISIONS = Counter("aisre_guard_decisions_seen_total", "Guard decisions received", ["decision"])

# Cap for conclusions extracted by the follow-up pass.
FINALIZE_MAX_CONFIDENCE = 0.3

HELP = """Commands:
/status - open incidents
/mode - remediation mode and playbook catalog
/playbooks - active playbooks
/investigate <question> - ad-hoc investigation (plain text works too)
/reinvestigate <incident id> - run the investigation of an open incident again"""

Publish = Callable[[Notification], Awaitable[None]]


# Approvals decided in the web console (and its API) carry this adapter name.
CONSOLE_ADAPTER = "web"


class Engine:
    def __init__(self, repo: Repo, holmes: Holmes, guard: Guard, publish: Publish, max_parallel: int, approval_ttl: int,
                 local_cluster: str = "", console_url: str = ""):
        self.local_cluster = local_cluster
        self.console_url = console_url.rstrip("/")
        # Approve/Reject buttons in chat and decisions from chat adapters. Off in the community
        # edition (approvals in the console); an extension (enterprise license) turns it on.
        self.chat_approvals = False
        self.repo = repo
        self.holmes = holmes
        self.guard = guard
        self._publish = publish
        self.approval_ttl = approval_ttl
        self.sem = asyncio.Semaphore(max_parallel)
        self.tasks: set[asyncio.Task] = set()

    async def publish(self, n: Notification) -> None:
        if n.incident_id:
            inc = await self.repo.get(n.incident_id)
            if inc and inc.is_eval:
                log.info("evaluation incident %s: %s not sent to chat", n.incident_id, n.type)
                return
        await self._publish(n)

    def spawn(self, coro: Awaitable[Any]) -> None:
        task = asyncio.create_task(coro)
        self.tasks.add(task)
        task.add_done_callback(self.tasks.discard)

    async def handle(self, ev: Event) -> None:
        handlers = {
            "alert": self.on_alert,
            "check": self.on_check,
            "chat_command": self.on_chat,
            "approval": self.on_approval,
        }
        await handlers[ev.kind](ev)

    async def resume(self) -> None:
        """Restarts investigations interrupted by a restart."""
        for inc in await self.repo.list_status(["new", "investigating"]):
            log.info("resuming investigation of %s", inc.id)
            self.spawn(self.investigate(inc.id))

    # Alerts

    async def on_alert(self, ev: Event) -> None:
        status = ev.payload.get("status")
        inc = await self.repo.find_open(ev.incident_key)

        if status == "resolved":
            if not inc:
                return
            await self.repo.upsert_alert(inc.id, ev)
            inc = await self.repo.get(inc.id)
            if all(a.get("status") == "resolved" for a in inc.alerts):
                await self.repo.set_status(inc.id, "resolved")
                await self.publish(
                    Notification(
                        type="incident_resolved",
                        incident_id=inc.id,
                        title=f"Resolved: {inc.title}",
                        text=f"All alerts of {inc.target.describe()} are resolved.",
                        severity="ok",
                    )
                )
            return

        if inc:
            result = await self.repo.upsert_alert(inc.id, ev)
            log.info("incident %s: alert %s %s", inc.id, ev.payload.get("alertname"), result)
            return

        title = f"{ev.payload.get('alertname')} on {ev.target.workload or ev.target.pod or ev.target.node or ev.target.host or ev.target.cluster}"
        inc = await self.repo.create(ev, title)
        INCIDENTS.labels("alert").inc()
        annotations = ev.payload.get("annotations") or {}
        await self.publish(
            Notification(
                type="incident_opened",
                incident_id=inc.id,
                title=f"Incident: {title}",
                text="\n".join(
                    x
                    for x in (
                        annotations.get("summary") or annotations.get("description") or "",
                        f"Target: {ev.target.describe()}",
                        "Investigating...",
                    )
                    if x
                ),
                severity=(ev.payload.get("labels") or {}).get("severity", "warning"),
            )
        )
        self.spawn(self.investigate(inc.id))

    async def investigate(self, incident_id: str) -> None:
        async with self.sem:
            inc = await self.repo.get(incident_id)
            if not inc or inc.status not in ("new", "investigating"):
                return
            await self.repo.set_status(inc.id, "investigating")
            alert = inc.firing_alert() or {}
            try:
                playbooks = await self.guard.playbooks(alert.get("alertname", ""), inc.target.kind)
            except Exception as exc:  # The investigation is still useful without playbooks.
                log.warning("cannot list playbooks: %r", exc)
                playbooks = []

            similar = []
            try:
                # Evaluations must not see answers from earlier runs.
                if not inc.is_eval:
                    similar = await self.repo.similar(inc)
            except Exception as exc:
                log.warning("similar incidents lookup failed: %r", exc)
            if similar:
                await self.repo.log(inc.id, "similar_incidents", {"ids": [s["id"] for s in similar]})
            started = time.monotonic()
            try:
                result = await self.holmes.ask(
                    investigation_prompt(inc, playbooks, local_cluster=self.local_cluster, similar=similar), SYSTEM_PROMPT)
            except HolmesError as exc:
                INVESTIGATIONS.labels("error").inc()
                log.error("investigation of %s failed: %s", inc.id, exc)
                await self.repo.set_status(inc.id, "investigation_failed")
                await self.publish(
                    Notification(
                        type="rca",
                        incident_id=inc.id,
                        title=f"Investigation failed: {inc.title}",
                        text=str(exc),
                        severity="warning",
                    )
                )
                return
            seconds = time.monotonic() - started
            INVESTIGATION_SECONDS.observe(seconds)
            tool_calls = result.get("tool_calls") or []
            await self.repo.log(inc.id, "investigation_stats", {
                "seconds": round(seconds, 1),
                "tool_calls": len(tool_calls),
                "tools": sorted({t.get("tool_name") or "?" for t in tool_calls}),
            })

            names = {p["name"] for p in playbooks}
            parsed = parse_investigation(result.get("analysis", ""), names)
            if not parsed.structured and result.get("conversation_history"):
                # Small models often forget the final JSON; ask for it in the
                # same conversation, without new tool calls.
                try:
                    final = await self.holmes.ask(FINALIZE_PROMPT, SYSTEM_PROMPT, result["conversation_history"])
                    summary = parse_investigation(final.get("analysis", ""), names)
                    if summary.structured:
                        # The main pass did not conclude: never act on this
                        # summary and never present it as confident.
                        summary.analysis = parsed.analysis
                        summary.playbook = None
                        summary.confidence = min(summary.confidence or 0.0, FINALIZE_MAX_CONFIDENCE)
                        if (summary.root_cause or "").strip().lower() in ("", "unknown"):
                            summary.root_cause = None
                        parsed = summary
                except HolmesError as exc:
                    log.warning("finalize of %s failed: %s", inc.id, exc)
            INVESTIGATIONS.labels("structured" if parsed.structured else "unstructured").inc()
            await self.repo.save_rca(inc.id, parsed.analysis, parsed.root_cause, parsed.confidence, parsed.playbook)
            if playbooks:
                await self.repo.log(inc.id, "playbook_choice", {
                    "offered": sorted(names), "chosen": parsed.playbook, "reason": parsed.playbook_reason})

            # Chat gets the short report; the full analysis is in the web console.
            report = parsed.chat_report or parsed.analysis
            lines = []
            # Capable models write their own root cause section.
            if parsed.root_cause and "root cause" not in report.lower():
                lines.append(f"Root cause: {parsed.root_cause}")
            if parsed.confidence is not None:
                lines.append(f"Confidence: {parsed.confidence:.0%}")
            lines += ["", report]
            await self.publish(
                Notification(type="rca", incident_id=inc.id, title=f"RCA: {inc.title}", text="\n".join(lines).strip())
            )

            if parsed.playbook and inc.is_eval:
                await self.repo.log(inc.id, "remediation_skipped", {"reason": "evaluation incident"})
            elif parsed.playbook:
                await self.remediate(inc, parsed.playbook, parsed.playbook_reason)

    async def remediate(self, inc: Incident, playbook: str, why: str | None) -> None:
        alert = inc.firing_alert() or {}
        request = {
            "incident_id": inc.id,
            "playbook": playbook,
            "target": inc.target.model_dump(exclude_none=True),
            "alert": {"alertname": alert.get("alertname", ""), "labels": alert.get("labels", {})},
        }
        try:
            decision = await self.guard.evaluate(request)
        except (GuardError, Exception) as exc:
            log.error("guard evaluation failed: %r", exc)
            await self.publish(
                Notification(
                    type="proposal",
                    incident_id=inc.id,
                    title=f"Remediation not evaluated: {inc.title}",
                    text=f"Action Guard is unavailable: {exc}",
                    severity="warning",
                )
            )
            return
        DECISIONS.labels(decision["decision"]).inc()
        await self.repo.save_decision(inc.id, decision)
        action = describe_action(decision)
        reason_line = f"Why this playbook: {why}" if why else ""
        kind = decision["decision"]

        if kind == "propose":
            await self.publish(
                Notification(
                    type="proposal",
                    incident_id=inc.id,
                    title=f"Proposed remediation: {playbook}",
                    text="\n".join(x for x in (action, reason_line, f"Not executed: {decision['reason']}") if x),
                )
            )
        elif kind == "approval_required":
            approval_id = await self.repo.create_approval(inc.id, playbook, request, self.approval_ttl)
            await self.repo.set_status(inc.id, "awaiting_approval")
            actions, where = [], self.console_hint()
            if self.chat_approvals:
                actions = [
                    NotificationAction(label="Approve", decision="approve", approval_id=approval_id),
                    NotificationAction(label="Reject", decision="reject", approval_id=approval_id),
                ]
                where = ""
            await self.publish(
                Notification(
                    type="approval_request",
                    incident_id=inc.id,
                    title=f"Approval needed: {playbook} (risk {decision.get('risk')})",
                    text="\n".join(x for x in (action, reason_line, f"Approval needed: {decision['reason']}", where) if x),
                    severity="warning",
                    actions=actions,
                )
            )
        elif kind == "execute":
            await self.execute(inc, request, None)
        else:
            await self.publish(
                Notification(
                    type="proposal",
                    incident_id=inc.id,
                    title=f"Remediation denied: {playbook}",
                    text=decision["reason"],
                    severity="warning",
                )
            )

    async def execute(self, inc: Incident, request: dict[str, Any], approved_by: str | None) -> None:
        if approved_by:
            request = {**request, "approved_by": approved_by}
        await self.repo.set_status(inc.id, "remediating")
        await self.publish(
            Notification(
                type="action_result",
                incident_id=inc.id,
                title=f"Remediation started: {request['playbook']}",
                text=describe_action(await self._evaluation_hint(request)) or "Running prechecks and a dry run.",
                progress=True,
            )
        )
        try:
            code, body = await self.guard.execute(request)
        except Exception as exc:
            code, body = 0, {"status": "error", "reason": f"Action Guard unavailable: {exc!r}"}
        await self.repo.log(inc.id, "execute", {"code": code, "status": body.get("status"), "reason": body.get("reason"),
                                                "steps": body.get("steps")})
        status = body.get("status", "unknown")
        new_status = {"succeeded": "remediated", "refused": "diagnosed"}.get(status, "remediation_failed")
        if code != 200:
            new_status = "diagnosed"
        await self.repo.set_status(inc.id, new_status)
        lines = [f"Result: {status}. {body.get('reason') or body.get('evaluation', {}).get('reason', '')}".strip()]
        for st in body.get("steps") or []:
            lines.append(f"- {'ok' if st.get('ok') else 'FAILED'}: {st.get('name')}: {st.get('output', '')}")
        await self.publish(
            Notification(
                type="action_result",
                incident_id=inc.id,
                title=f"Remediation {status}: {request['playbook']}",
                text="\n".join(lines),
                severity="ok" if status == "succeeded" else "warning",
            )
        )

    async def _evaluation_hint(self, request: dict[str, Any]) -> dict[str, Any]:
        try:
            return await self.guard.evaluate(request)
        except Exception:
            return {}

    # Approvals

    def console_hint(self) -> str:
        if self.console_url:
            return f"Approve or reject in the console: {self.console_url}/approvals"
        return "Approve or reject in the web console (Approvals)."

    async def on_approval(self, ev: Event) -> None:
        decision = ApprovalDecision.model_validate(ev.payload)
        ref = decision.ref
        if ref.adapter != CONSOLE_ADAPTER and not self.chat_approvals:
            await self.answer(ref, "Approvals in chat need the enterprise edition. " + self.console_hint())
            return
        approval = await self.repo.get_approval(decision.approval_id)
        if not approval:
            await self.answer(ref, "Approval not found.")
            return
        if approval.status != "pending":
            await self.answer(ref, f"Approval is already {approval.status}.")
            return
        if approval.expires_at < datetime.now(timezone.utc):
            await self.repo.decide_approval(approval.id, "expired", "system")
            await self.answer(ref, "Approval has expired; the incident needs a new evaluation.")
            return

        who = ref.user_name or ref.user_id or "unknown"
        status = "approved" if decision.decision == "approve" else "rejected"
        if not await self.repo.decide_approval(approval.id, status, f"{ref.adapter}:{who}"):
            await self.answer(ref, "Approval was decided concurrently.")
            return
        inc = await self.repo.get(approval.incident_id)
        if status == "rejected":
            await self.repo.set_status(inc.id, "diagnosed")
            await self.publish(
                Notification(
                    type="action_result",
                    incident_id=inc.id,
                    title=f"Rejected: {approval.playbook}",
                    text=f"Rejected by {who}.",
                )
            )
            return
        # Execution waits for verification; do not block the event consumer.
        self.spawn(self.execute(inc, approval.request, f"{ref.adapter}:{who}"))

    # Scheduled checks

    async def on_check(self, ev: Event) -> None:
        self.spawn(self.run_check(ev))

    async def run_check(self, ev: Event) -> None:
        name = ev.payload.get("name", "check")
        async with self.sem:
            try:
                result = await self.holmes.ask(ev.payload.get("query", ""), CHECK_SYSTEM_PROMPT)
            except HolmesError as exc:
                log.error("check %s failed: %s", name, exc)
                return
        healthy, summary = parse_check(result.get("analysis", ""))
        log.info("check %s healthy=%s", name, healthy)
        if healthy is False:
            await self.publish(
                Notification(type="rca", title=f"Health check failed: {name}", text=summary, severity="warning")
            )

    # Chat

    async def answer(self, ref: ChatRef, text: str, title: str = "", progress: bool = False) -> None:
        await self.publish(Notification(type="answer", title=title, text=text, reply_to=ref, progress=progress))

    async def on_chat(self, ev: Event) -> None:
        msg = ChatMessage.model_validate(ev.payload)
        text = msg.text.strip()
        command, _, arg = text.partition(" ")
        command = command.split("@")[0].lower()

        if command in ("/start", "/help"):
            await self.answer(msg.ref, HELP)
        elif command == "/status":
            incidents = await self.repo.list_open()
            if not incidents:
                await self.answer(msg.ref, "No open incidents.")
            else:
                lines = [f"- [{i.status}] {i.title} ({i.id[:8]})" for i in incidents]
                await self.answer(msg.ref, "\n".join(lines), "Open incidents")
        elif command == "/reinvestigate":
            inc = await self.repo.find_by_prefix(arg.strip()) if arg.strip() else None
            if not inc:
                await self.answer(msg.ref, "Incident not found. Use /status to list open incidents.")
            elif inc.status in ("new", "investigating"):
                await self.answer(msg.ref, f"Incident {inc.id[:8]} is already being investigated.")
            else:
                await self.repo.set_status(inc.id, "new")
                await self.answer(msg.ref, f"Investigating {inc.title} again...", progress=True)
                self.spawn(self.investigate(inc.id))
        elif command == "/mode":
            try:
                st = await self.guard.status()
                await self.answer(
                    msg.ref,
                    f"SRE_MODE={st['mode']}, AUTOFIX_MAX_RISK={st['autofix_max_risk']}\n"
                    f"Catalog {st.get('catalog_version')} ({st.get('playbooks', 0)} playbooks), "
                    f"valid: {st.get('catalog_valid')}",
                )
            except Exception as exc:
                await self.answer(msg.ref, f"Action Guard is unavailable: {exc}")
        elif command == "/playbooks":
            try:
                pbs = await self.guard.playbooks()
                lines = [f"- {p['name']} ({p['risk']}{'' if p['enabled'] else ', disabled'}): {p['description']}" for p in pbs]
                await self.answer(msg.ref, "\n".join(lines) or "Catalog is empty.", "Playbooks")
            except Exception as exc:
                await self.answer(msg.ref, f"Action Guard is unavailable: {exc}")
        else:
            question = arg if command == "/investigate" else text
            if not question:
                await self.answer(msg.ref, HELP)
                return
            await self.answer(msg.ref, "Investigating, this can take a few minutes...", progress=True)
            self.spawn(self.ask(msg.ref, question))

    async def ask(self, ref: ChatRef, question: str) -> None:
        async with self.sem:
            try:
                result = await self.holmes.ask(question, CHAT_SYSTEM_PROMPT)
                await self.answer(ref, clean(result.get("analysis", "")) or "No answer.")
            except HolmesError as exc:
                await self.answer(ref, f"Investigation failed: {exc}")


def describe_action(decision: dict[str, Any]) -> str:
    action = decision.get("action") or {}
    if not action:
        return ""
    params = ", ".join(f"{k}={v}" for k, v in sorted((action.get("params") or {}).items()))
    lines = [f"Action: {action.get('primitive')}({params})"]
    if decision.get("description"):
        lines.append(decision["description"])
    if decision.get("risk"):
        lines.append(f"Risk: {decision['risk']}")
    return "\n".join(lines)
