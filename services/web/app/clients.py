"""Clients for the other services: ingest (actions), guard (status) and health probes."""

from __future__ import annotations

import asyncio
import logging
from typing import Any

import httpx

from aisre_common.models import ApprovalDecision, ChatMessage, ChatRef

from .config import Config

log = logging.getLogger("web.clients")

WEB_ADAPTER = "web"


class Services:
    def __init__(self, cfg: Config):
        self.cfg = cfg
        headers = {"Authorization": f"Bearer {cfg.ingest_token}"} if cfg.ingest_token else {}
        self.ingest = httpx.AsyncClient(base_url=cfg.ingest_url, headers=headers, timeout=10)
        self.guard = httpx.AsyncClient(base_url=cfg.guard_url, timeout=5)
        self.probe = httpx.AsyncClient(timeout=3, headers={"Accept": "application/json"})
        self.hub = None
        if cfg.tool_hub_url:
            self.hub = httpx.AsyncClient(base_url=cfg.tool_hub_url, timeout=5,
                                         headers={"Authorization": f"Bearer {cfg.tool_hub_token}"})

    # Actions go through ingest, the same path as chat, so the controller applies
    # them with the same rules (one decision per approval, audit in its timeline).

    async def send_command(self, username: str, chat_id: str, text: str) -> None:
        msg = ChatMessage(ref=ChatRef(adapter=WEB_ADAPTER, chat_id=chat_id, user_id=username, user_name=username),
                          text=text)
        (await self.ingest.post("/v1/chat", content=msg.model_dump_json())).raise_for_status()

    async def decide(self, username: str, approval_id: str, decision: str) -> None:
        body = ApprovalDecision(
            approval_id=approval_id, decision=decision,
            ref=ChatRef(adapter=WEB_ADAPTER, chat_id="web", user_id=username, user_name=username),
        )
        (await self.ingest.post("/v1/approvals", content=body.model_dump_json())).raise_for_status()

    async def guard_status(self) -> dict[str, Any] | None:
        try:
            resp = await self.guard.get("/v1/status")
            resp.raise_for_status()
            return resp.json()
        except Exception as exc:
            log.warning("guard status: %r", exc)
            return None

    async def guard_playbooks(self) -> list[dict[str, Any]]:
        try:
            resp = await self.guard.get("/v1/playbooks")
            resp.raise_for_status()
            return resp.json()["playbooks"]
        except Exception as exc:
            log.warning("guard playbooks: %r", exc)
            return []

    async def hub_targets(self) -> list[dict[str, Any]] | None:
        """Agents connected to the Tool Hub; None when the hub is not configured or unreachable."""
        if not self.hub:
            return None
        try:
            resp = await self.hub.get("/v1/targets")
            resp.raise_for_status()
            return resp.json().get("targets") or []
        except Exception as exc:
            log.warning("tool hub targets: %r", exc)
            return None

    async def _check(self, name: str, url: str) -> dict[str, Any]:
        try:
            resp = await self.probe.get(url)
            ok = resp.status_code < 400
            return {"name": name, "ok": ok, "detail": "" if ok else f"HTTP {resp.status_code}"}
        except Exception as exc:
            return {"name": name, "ok": False, "detail": type(exc).__name__}

    async def health(self, extra: list[dict[str, Any]]) -> list[dict[str, Any]]:
        checks = await asyncio.gather(
            self._check("Event Ingest", f"{self.cfg.ingest_url}/healthz"),
            self._check("Action Guard", f"{self.cfg.guard_url}/healthz"),
            self._check("HolmesGPT", f"{self.cfg.holmes_url}/healthz"),
            self._check("LLM Gateway", f"{self.cfg.litellm_url}/health/liveliness"),
            *([self._check("Tool Hub", f"{self.cfg.tool_hub_url}/healthz")] if self.cfg.tool_hub_url else []),
        )
        return list(checks) + extra
