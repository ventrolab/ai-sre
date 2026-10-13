"""REST API for automation (/api/v1), authenticated with admin-issued API tokens.

    curl -H "Authorization: Bearer aisre_..." https://console/api/v1/agents
"""

from __future__ import annotations

import logging
from typing import Any, Awaitable, Callable

from fastapi import APIRouter, Depends, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

from . import fleet
from .config import Config

log = logging.getLogger("web.api")


class ApiError(Exception):
    def __init__(self, status: int, detail: str):
        self.status, self.detail = status, detail


class EnrollmentIn(BaseModel):
    name: str
    note: str = ""
    ttl_hours: int = Field(default=24, ge=1, le=fleet.MAX_TTL_HOURS)
    replace: bool = False


def _agent_json(a: dict[str, Any]) -> dict[str, Any]:
    keys = ("name", "kind", "state", "labels", "tools", "version", "remote", "auth", "token_id", "cert_serial",
            "cert_not_after", "revoked_at", "connected_at", "last_seen_at", "disconnected_at", "first_seen_at")
    return {k: a.get(k) for k in keys}


def _incident_json(i: dict[str, Any]) -> dict[str, Any]:
    keys = ("id", "key", "status", "title", "confidence", "playbook", "target", "alert_count", "created_at",
            "updated_at", "resolved_at")
    return {k: i.get(k) for k in keys}


def build_router(cfg: Config, db: Callable[[], Any],
                 system_status: Callable[[], Awaitable[dict[str, Any]]] | None = None) -> APIRouter:
    router = APIRouter(prefix="/api/v1")

    def scope(required: str):
        async def dep(request: Request) -> dict:
            auth = request.headers.get("authorization", "")
            if not auth.lower().startswith("bearer "):
                raise ApiError(401, "Bearer API token required")
            token = await db().api_token(auth[7:].strip())
            if not token:
                raise ApiError(401, "invalid, expired or revoked API token")
            if required not in token["scopes"]:
                raise ApiError(403, f"token lacks scope {required}")
            return token
        return dep

    async def audit(token: dict, action: str, target: str | None, detail: dict | None = None) -> None:
        await db().audit(f"token:{token['name']}", action, target, {**(detail or {}), "via": "api"})

    @router.get("/status")
    async def status(token: dict = Depends(scope("status:read"))):
        if system_status is None:
            raise ApiError(503, "status not available")
        st = await system_status()
        d, guard = st["data"], st["guard"] or {}
        return {
            "version": cfg.version,
            "cluster": cfg.cluster_name,
            "remediation": {"mode": guard.get("mode"), "max_risk": guard.get("autofix_max_risk"),
                            "max_actions_per_hour": guard.get("autofix_max_actions_per_hour"),
                            "playbooks": guard.get("playbooks"), "available": st["guard"] is not None},
            "llm": {"provider": cfg.llm_provider, "fallbacks": cfg.llm_fallbacks},
            "incidents": {"open": st["open_total"], "by_status": d["by_status"], "opened_24h": d["opened_24h"],
                          "opened_7d": d["opened_7d"], "mttd_seconds": d["mttd"], "mttr_seconds": d["mttr"],
                          "top_alerts_7d": d["top_alerts"]},
            "pending_approvals": d["pending_approvals"],
            "components": st["health"],
        }

    @router.get("/incidents")
    async def list_incidents(status: str = "open", q: str = "", limit: int = 50,
                             token: dict = Depends(scope("status:read"))):
        rows = await db().incidents(status=status, q=q.strip(), limit=max(1, min(limit, 500)))
        return {"incidents": [_incident_json(i) for i in rows]}

    @router.get("/incidents/{incident_id}")
    async def get_incident(incident_id: str, token: dict = Depends(scope("status:read"))):
        inc = await db().incident(incident_id)
        if not inc:
            raise ApiError(404, "incident not found")
        return {**_incident_json(inc), "kind": inc.get("kind"), "alerts": inc.get("alerts"),
                "root_cause": inc.get("root_cause"), "analysis": inc.get("analysis"),
                "decision": inc.get("decision"), "events": inc.get("events"), "approvals": inc.get("approvals"),
                "alert_count": len(inc.get("alerts") or [])}

    @router.get("/approvals")
    async def list_approvals(token: dict = Depends(scope("status:read"))):
        pending, history = await db().pending_approvals(), await db().approval_history()
        return {"pending": pending, "history": history}

    @router.get("/agents")
    async def list_agents(token: dict = Depends(scope("agents:read"))):
        rows = await db().agents()
        if rows is None:
            raise ApiError(503, "agent registry not available")
        return {"agents": [_agent_json(a) for a in rows]}

    @router.post("/agents/enrollments", status_code=201)
    async def create_enrollment(body: EnrollmentIn, token: dict = Depends(scope("agents:enroll"))):
        if not fleet.valid_name(body.name):
            raise ApiError(400, "name must be a lowercase DNS label (a-z, 0-9, '.', '-'), at most 63 characters")
        if not await db().agent_tables():
            raise ApiError(503, "agent registry not available")
        try:
            e = await db().create_enrollment(body.name, f"token:{token['name']}", body.ttl_hours, body.note,
                                             body.replace)
        except ValueError as exc:
            raise ApiError(409, f"{exc}; pass replace=true to re-enroll it")
        await audit(token, "agent_enroll_token", body.name, {"ttl_hours": body.ttl_hours, "replace": body.replace})
        return {"name": e["name"], "join_token": e["token"], "expires_at": e["expires_at"],
                **fleet.install_hint(cfg, e["token"])}

    @router.post("/agents/{name}/revoke")
    async def revoke(name: str, token: dict = Depends(scope("agents:revoke"))):
        revoked = await db().revoke_agent(name, f"token:{token['name']}")
        if not revoked:
            raise ApiError(404, "unknown agent or no credential recorded")
        await audit(token, "agent_revoke", name, revoked)
        return {"name": name, "revoked_serial": revoked["serial"], "revoked_tokens": revoked["tokens"]}

    return router


async def api_error_handler(_: Request, exc: ApiError) -> JSONResponse:
    return JSONResponse({"error": exc.detail}, status_code=exc.status)
