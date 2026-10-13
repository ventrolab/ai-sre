"""HTTP clients for HolmesGPT and the Action Guard."""

from __future__ import annotations

import logging
from typing import Any

import httpx

log = logging.getLogger("controller.clients")


class HolmesError(Exception):
    pass


class Holmes:
    def __init__(self, url: str, model: str, timeout: int):
        self.model = model
        # HolmesGPT passes request headers to its tools and refuses to run
        # kubectl when a header contains shell metacharacters, such as the
        # default "Accept: */*".
        self.http = httpx.AsyncClient(
            base_url=url,
            timeout=httpx.Timeout(timeout, connect=10),
            headers={"Accept": "application/json"},
        )

    async def ask(
        self, question: str, system: str | None = None, history: list[dict] | None = None
    ) -> dict[str, Any]:
        body: dict[str, Any] = {"ask": question, "model": self.model}
        if system:
            body["additional_system_prompt"] = system
        if history:
            body["conversation_history"] = history
        try:
            resp = await self.http.post("/api/chat", json=body)
        except httpx.HTTPError as exc:
            raise HolmesError(f"holmes request failed: {exc!r}") from exc
        if resp.status_code != 200:
            raise HolmesError(f"holmes returned {resp.status_code}: {resp.text[:500]}")
        return resp.json()


class GuardError(Exception):
    pass


class Guard:
    def __init__(self, url: str):
        self.http = httpx.AsyncClient(base_url=url, timeout=30)

    async def status(self) -> dict[str, Any]:
        resp = await self.http.get("/v1/status")
        resp.raise_for_status()
        return resp.json()

    async def playbooks(self, alertname: str = "", target_kind: str = "") -> list[dict[str, Any]]:
        params = {k: v for k, v in {"alertname": alertname, "target_kind": target_kind}.items() if v}
        resp = await self.http.get("/v1/playbooks", params=params)
        resp.raise_for_status()
        return resp.json()["playbooks"]

    async def evaluate(self, request: dict[str, Any]) -> dict[str, Any]:
        resp = await self.http.post("/v1/evaluate", json=request)
        if resp.status_code != 200:
            raise GuardError(f"evaluate returned {resp.status_code}: {resp.text[:500]}")
        return resp.json()

    async def execute(self, request: dict[str, Any]) -> tuple[int, dict[str, Any]]:
        # Execution includes verification, which can take many minutes.
        resp = await self.http.post("/v1/execute", json=request, timeout=1500)
        return resp.status_code, resp.json()
