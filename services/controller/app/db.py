"""Incident storage in PostgreSQL."""

from __future__ import annotations

import asyncio
import json
import logging
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from typing import Any

import asyncpg

from aisre_common import similar
from aisre_common.models import Event, Target

log = logging.getLogger("controller.db")

SCHEMA = """
create table if not exists incidents (
    id uuid primary key,
    key text not null,
    kind text not null,
    status text not null,
    title text not null,
    target jsonb not null,
    alerts jsonb not null default '[]',
    analysis text,
    root_cause text,
    confidence real,
    playbook text,
    decision jsonb,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),
    resolved_at timestamptz
);
create index if not exists incidents_open_key on incidents (key)
    where status not in ('resolved', 'closed');

-- Full-text index for "similar past incidents".
create index if not exists incidents_fts on incidents using gin (
    to_tsvector('simple', title || ' ' || coalesce(root_cause, '') || ' ' || alerts::text));  -- = similar.DOCUMENT

create table if not exists incident_events (
    id bigserial primary key,
    incident_id uuid not null references incidents (id) on delete cascade,
    ts timestamptz not null default now(),
    type text not null,
    data jsonb not null default '{}'
);
create index if not exists incident_events_incident on incident_events (incident_id, ts);

create table if not exists approvals (
    id uuid primary key,
    incident_id uuid not null references incidents (id) on delete cascade,
    playbook text not null,
    request jsonb not null,
    status text not null default 'pending',
    requested_at timestamptz not null default now(),
    expires_at timestamptz not null,
    decided_at timestamptz,
    decided_by text
);
"""

OPEN_STATUSES = ("new", "investigating", "diagnosed", "awaiting_approval", "remediating", "remediated",
                 "remediation_failed", "investigation_failed")


@dataclass
class Incident:
    id: str
    key: str
    kind: str
    status: str
    title: str
    target: Target
    alerts: list[dict[str, Any]] = field(default_factory=list)
    analysis: str | None = None
    root_cause: str | None = None
    confidence: float | None = None
    playbook: str | None = None
    created_at: datetime | None = None

    @property
    def is_eval(self) -> bool:
        """Created by the evaluation runner (`evals/`): no chat, no remediation, no hints."""
        return any(similar.EVAL_LABEL in (a.get("labels") or {}) for a in self.alerts)

    def firing_alert(self) -> dict[str, Any] | None:
        for alert in self.alerts:
            if alert.get("status") == "firing":
                return alert
        return self.alerts[0] if self.alerts else None


@dataclass
class Approval:
    id: str
    incident_id: str
    playbook: str
    request: dict[str, Any]
    status: str
    expires_at: datetime


def alert_record(ev: Event) -> dict[str, Any]:
    p = ev.payload
    return {
        "fingerprint": ev.fingerprint,
        "alertname": p.get("alertname"),
        "status": p.get("status"),
        "labels": p.get("labels", {}),
        "annotations": p.get("annotations", {}),
        "starts_at": p.get("starts_at"),
    }


def _row_to_incident(row: asyncpg.Record) -> Incident:
    return Incident(
        id=str(row["id"]),
        key=row["key"],
        kind=row["kind"],
        status=row["status"],
        title=row["title"],
        target=Target.model_validate(json.loads(row["target"])),
        alerts=json.loads(row["alerts"]),
        analysis=row["analysis"],
        root_cause=row["root_cause"],
        confidence=row["confidence"],
        playbook=row["playbook"],
        created_at=row["created_at"],
    )


class Repo:
    def __init__(self, pool: asyncpg.Pool):
        self.pool = pool

    @classmethod
    async def connect(cls, url: str, attempts: int = 30, delay: float = 2.0) -> "Repo":
        # PostgreSQL may still be starting when the controller comes up.
        for attempt in range(1, attempts + 1):
            try:
                pool = await asyncpg.create_pool(url, min_size=1, max_size=5)
                break
            except (OSError, asyncpg.PostgresError) as exc:
                if attempt == attempts:
                    raise
                log.warning("database not ready (%s), retry %d/%d", exc, attempt, attempts)
                await asyncio.sleep(delay)
        async with pool.acquire() as conn:
            await conn.execute(SCHEMA)
        return cls(pool)

    async def find_open(self, key: str) -> Incident | None:
        row = await self.pool.fetchrow(
            "select * from incidents where key = $1 and status = any($2::text[]) order by created_at desc limit 1",
            key,
            list(OPEN_STATUSES),
        )
        return _row_to_incident(row) if row else None

    async def get(self, incident_id: str) -> Incident | None:
        row = await self.pool.fetchrow("select * from incidents where id = $1", uuid.UUID(incident_id))
        return _row_to_incident(row) if row else None

    async def similar(self, inc: Incident, limit: int = 3) -> list[dict[str, Any]]:
        """Diagnosed past incidents that share alert names or words with this one."""
        query = similar.tsquery(inc.title, inc.alerts, inc.target.model_dump())
        if not query:
            return []
        rows = await self.pool.fetch(similar.SQL, uuid.UUID(inc.id), query, limit)
        return [{**dict(r), "id": str(r["id"])} for r in rows]

    async def find_by_prefix(self, prefix: str) -> Incident | None:
        """Finds an open incident by the id prefix shown in chat."""
        rows = await self.pool.fetch(
            "select * from incidents where id::text like $1 and status = any($2::text[]) limit 2",
            prefix.lower() + "%",
            list(OPEN_STATUSES),
        )
        return _row_to_incident(rows[0]) if len(rows) == 1 else None

    async def create(self, ev: Event, title: str, kind: str = "alert") -> Incident:
        incident_id = uuid.uuid4()
        alerts = [alert_record(ev)] if ev.kind == "alert" else []
        row = await self.pool.fetchrow(
            """insert into incidents (id, key, kind, status, title, target, alerts)
               values ($1, $2, $3, 'new', $4, $5, $6) returning *""",
            incident_id,
            ev.incident_key or str(incident_id),
            kind,
            title,
            ev.target.model_dump_json(),
            json.dumps(alerts),
        )
        await self.log(str(incident_id), "opened", {"event_id": ev.id})
        return _row_to_incident(row)

    async def upsert_alert(self, incident_id: str, ev: Event) -> str:
        """Adds or updates an alert of an incident. Returns added, updated or unchanged."""
        record = alert_record(ev)
        async with self.pool.acquire() as conn, conn.transaction():
            row = await conn.fetchrow(
                "select alerts from incidents where id = $1 for update", uuid.UUID(incident_id)
            )
            alerts = json.loads(row["alerts"])
            result = "added"
            for i, alert in enumerate(alerts):
                if alert.get("fingerprint") == record["fingerprint"]:
                    result = "unchanged" if alert.get("status") == record["status"] else "updated"
                    alerts[i] = record
                    break
            else:
                alerts.append(record)
            await conn.execute(
                "update incidents set alerts = $2, updated_at = now() where id = $1",
                uuid.UUID(incident_id),
                json.dumps(alerts),
            )
        if result != "unchanged":
            await self.log(incident_id, f"alert_{result}", {"alertname": record["alertname"], "status": record["status"]})
        return result

    async def set_status(self, incident_id: str, status: str) -> None:
        resolved = status in ("resolved", "closed")
        await self.pool.execute(
            """update incidents set status = $2, updated_at = now(),
               resolved_at = case when $3 then now() else resolved_at end where id = $1""",
            uuid.UUID(incident_id),
            status,
            resolved,
        )
        await self.log(incident_id, "status", {"status": status})

    async def save_rca(
        self, incident_id: str, analysis: str, root_cause: str | None, confidence: float | None, playbook: str | None
    ) -> None:
        await self.pool.execute(
            """update incidents set analysis = $2, root_cause = $3, confidence = $4, playbook = $5,
               status = 'diagnosed', updated_at = now() where id = $1""",
            uuid.UUID(incident_id),
            analysis,
            root_cause,
            confidence,
            playbook,
        )
        await self.log(incident_id, "diagnosed", {"root_cause": root_cause, "playbook": playbook})

    async def save_decision(self, incident_id: str, decision: dict[str, Any]) -> None:
        await self.pool.execute(
            "update incidents set decision = $2, updated_at = now() where id = $1",
            uuid.UUID(incident_id),
            json.dumps(decision),
        )
        await self.log(incident_id, "guard_decision", decision)

    async def list_status(self, statuses: list[str]) -> list[Incident]:
        rows = await self.pool.fetch(
            "select * from incidents where status = any($1::text[]) order by created_at", statuses
        )
        return [_row_to_incident(r) for r in rows]

    async def list_open(self, limit: int = 20) -> list[Incident]:
        rows = await self.pool.fetch(
            "select * from incidents where status = any($1::text[]) order by created_at desc limit $2",
            list(OPEN_STATUSES),
            limit,
        )
        return [_row_to_incident(r) for r in rows]

    async def create_approval(self, incident_id: str, playbook: str, request: dict[str, Any], ttl: int) -> str:
        approval_id = uuid.uuid4()
        await self.pool.execute(
            "insert into approvals (id, incident_id, playbook, request, expires_at) values ($1, $2, $3, $4, $5)",
            approval_id,
            uuid.UUID(incident_id),
            playbook,
            json.dumps(request),
            datetime.now(timezone.utc) + timedelta(seconds=ttl),
        )
        await self.log(incident_id, "approval_requested", {"approval_id": str(approval_id), "playbook": playbook})
        return str(approval_id)

    async def get_approval(self, approval_id: str) -> Approval | None:
        try:
            key = uuid.UUID(approval_id)
        except ValueError:
            return None
        row = await self.pool.fetchrow("select * from approvals where id = $1", key)
        if not row:
            return None
        return Approval(
            id=str(row["id"]),
            incident_id=str(row["incident_id"]),
            playbook=row["playbook"],
            request=json.loads(row["request"]),
            status=row["status"],
            expires_at=row["expires_at"],
        )

    async def decide_approval(self, approval_id: str, status: str, by: str) -> bool:
        """Atomically moves a pending approval to a final status."""
        result = await self.pool.execute(
            """update approvals set status = $2, decided_by = $3, decided_at = now()
               where id = $1 and status = 'pending'""",
            uuid.UUID(approval_id),
            status,
            by,
        )
        return result.endswith(" 1")

    async def log(self, incident_id: str, type_: str, data: dict[str, Any]) -> None:
        await self.pool.execute(
            "insert into incident_events (incident_id, type, data) values ($1, $2, $3)",
            uuid.UUID(incident_id),
            type_,
            json.dumps(data, default=str),
        )
