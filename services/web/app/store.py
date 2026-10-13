"""PostgreSQL access: web tables plus read queries over incidents."""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import secrets
import uuid
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Any

import asyncpg

from aisre_common import similar

log = logging.getLogger("web.store")

SCHEMA = """
create table if not exists web_users (
    id uuid primary key,
    username text not null unique,
    display_name text,
    email text,
    source text not null,               -- local | oidc
    password_hash text,
    role text not null,
    disabled boolean not null default false,
    created_at timestamptz not null default now(),
    last_login_at timestamptz
);

create table if not exists web_audit (
    id bigserial primary key,
    ts timestamptz not null default now(),
    username text not null,
    action text not null,
    target text,
    detail jsonb not null default '{}'
);
create index if not exists web_audit_ts on web_audit (ts desc);

create table if not exists web_questions (
    id uuid primary key,
    username text not null,
    question text not null,
    status text not null default 'queued',  -- queued | running | done
    answer text,
    created_at timestamptz not null default now(),
    answered_at timestamptz
);
create index if not exists web_questions_user on web_questions (username, created_at desc);

-- API tokens for automation, issued by admins. Only the hash is stored.
create table if not exists api_tokens (
    id uuid primary key,
    name text not null,
    prefix text not null,                -- first characters, to recognise a token
    token_hash text not null unique,     -- hex SHA-256
    scopes text[] not null,
    created_by text not null,
    created_at timestamptz not null default now(),
    expires_at timestamptz,
    last_used_at timestamptz,
    revoked_at timestamptz
);
"""

# Same set as the controller's open statuses.
OPEN_STATUSES = ["new", "investigating", "diagnosed", "awaiting_approval", "remediating", "remediated",
                 "remediation_failed", "investigation_failed"]


@dataclass
class User:
    id: str
    username: str
    display_name: str | None
    email: str | None
    source: str
    password_hash: str | None
    role: str
    disabled: bool
    created_at: datetime | None = None
    last_login_at: datetime | None = None


def _user(row: asyncpg.Record | None) -> User | None:
    if not row:
        return None
    return User(
        id=str(row["id"]),
        username=row["username"],
        display_name=row["display_name"],
        email=row["email"],
        source=row["source"],
        password_hash=row["password_hash"],
        role=row["role"],
        disabled=row["disabled"],
        created_at=row["created_at"],
        last_login_at=row["last_login_at"],
    )


def _json(value: Any) -> Any:
    return json.loads(value) if isinstance(value, str) else value


class Store:
    def __init__(self, pool: asyncpg.Pool):
        self.pool = pool

    @classmethod
    async def connect(cls, url: str, attempts: int = 30) -> "Store":
        for attempt in range(1, attempts + 1):
            try:
                pool = await asyncpg.create_pool(url, min_size=1, max_size=5)
                break
            except (OSError, asyncpg.PostgresError) as exc:
                if attempt == attempts:
                    raise
                log.warning("database not ready (%s), retry %d/%d", exc, attempt, attempts)
                await asyncio.sleep(2)
        async with pool.acquire() as conn:
            await conn.execute(SCHEMA)
        return cls(pool)

    async def ping(self) -> bool:
        return await self.pool.fetchval("select 1") == 1

    # Users

    async def user_by_name(self, username: str) -> User | None:
        return _user(await self.pool.fetchrow("select * from web_users where username = $1", username))

    async def user_by_id(self, user_id: str) -> User | None:
        return _user(await self.pool.fetchrow("select * from web_users where id = $1", uuid.UUID(user_id)))

    async def list_users(self) -> list[User]:
        return [_user(r) for r in await self.pool.fetch("select * from web_users order by source, username")]

    async def count_local_users(self) -> int:
        return await self.pool.fetchval("select count(*) from web_users where source = 'local'")

    async def create_user(
        self, username: str, source: str, role: str, password_hash: str | None = None,
        display_name: str | None = None, email: str | None = None,
    ) -> User:
        row = await self.pool.fetchrow(
            """insert into web_users (id, username, source, role, password_hash, display_name, email)
               values ($1, $2, $3, $4, $5, $6, $7) returning *""",
            uuid.uuid4(), username, source, role, password_hash, display_name, email,
        )
        return _user(row)

    async def upsert_oidc_user(self, username: str, role: str, display_name: str | None, email: str | None) -> User:
        """OIDC users get their role from the identity provider on every login."""
        row = await self.pool.fetchrow(
            """insert into web_users (id, username, source, role, display_name, email, last_login_at)
               values ($1, $2, 'oidc', $3, $4, $5, now())
               on conflict (username) do update set role = excluded.role, display_name = excluded.display_name,
                   email = excluded.email, last_login_at = now()
               where web_users.source = 'oidc'
               returning *""",
            uuid.uuid4(), username, role, display_name, email,
        )
        return _user(row)

    async def touch_login(self, user_id: str) -> None:
        await self.pool.execute("update web_users set last_login_at = now() where id = $1", uuid.UUID(user_id))

    async def update_user(self, user_id: str, **fields: Any) -> None:
        allowed = {"role", "disabled", "password_hash", "display_name"}
        sets, args = [], [uuid.UUID(user_id)]
        for key, value in fields.items():
            if key not in allowed:
                raise ValueError(key)
            args.append(value)
            sets.append(f"{key} = ${len(args)}")
        if sets:
            await self.pool.execute(f"update web_users set {', '.join(sets)} where id = $1", *args)

    async def count_active_admins(self) -> int:
        return await self.pool.fetchval("select count(*) from web_users where role = 'admin' and not disabled")

    # Audit

    async def audit(self, username: str, action: str, target: str | None = None, detail: dict | None = None) -> None:
        await self.pool.execute(
            "insert into web_audit (username, action, target, detail) values ($1, $2, $3, $4)",
            username, action, target, json.dumps(detail or {}, default=str),
        )

    async def audit_log(self, limit: int = 200) -> list[dict]:
        rows = await self.pool.fetch("select * from web_audit order by ts desc limit $1", limit)
        return [{**dict(r), "detail": _json(r["detail"])} for r in rows]

    # Questions

    async def create_question(self, username: str, question: str) -> str:
        qid = uuid.uuid4()
        await self.pool.execute(
            "insert into web_questions (id, username, question) values ($1, $2, $3)", qid, username, question
        )
        return str(qid)

    async def update_question(self, qid: str, status: str, answer: str | None) -> None:
        await self.pool.execute(
            """update web_questions set status = $2,
                   answer = coalesce($3, answer),
                   answered_at = case when $2 = 'done' then now() else answered_at end
               where id = $1""",
            uuid.UUID(qid), status, answer,
        )

    async def questions(self, username: str, limit: int = 20) -> list[dict]:
        rows = await self.pool.fetch(
            "select * from web_questions where username = $1 order by created_at desc limit $2", username, limit
        )
        return [dict(r) for r in rows]

    async def question(self, qid: str, username: str) -> dict | None:
        row = await self.pool.fetchrow(
            "select * from web_questions where id = $1 and username = $2", uuid.UUID(qid), username
        )
        return dict(row) if row else None

    # Incidents (tables owned by the controller)

    async def has_incidents_table(self) -> bool:
        return bool(await self.pool.fetchval("select to_regclass('public.incidents') is not null"))

    async def dashboard(self) -> dict[str, Any]:
        if not await self.has_incidents_table():
            return {"by_status": {}, "opened_24h": 0, "opened_7d": 0, "pending_approvals": 0, "mttd": None,
                    "mttr": None, "per_day": [], "top_alerts": [], "recent": []}
        by_status = {r["status"]: r["n"] for r in await self.pool.fetch(
            "select status, count(*) n from incidents group by status")}
        opened_24h = await self.pool.fetchval("select count(*) from incidents where created_at > now() - interval '24 hours'")
        opened_7d = await self.pool.fetchval("select count(*) from incidents where created_at > now() - interval '7 days'")
        pending = await self.pool.fetchval(
            "select count(*) from approvals where status = 'pending' and expires_at > now()")
        # Time to diagnosis: first "diagnosed" event after opening, last 7 days.
        mttd = await self.pool.fetchval(
            """select extract(epoch from avg(d.ts - i.created_at)) from incidents i
               join lateral (select min(ts) ts from incident_events e
                             where e.incident_id = i.id and e.type = 'diagnosed') d on d.ts is not null
               where i.created_at > now() - interval '7 days'""")
        mttr = await self.pool.fetchval(
            """select extract(epoch from avg(resolved_at - created_at)) from incidents
               where resolved_at is not null and created_at > now() - interval '7 days'""")
        per_day = [dict(r) for r in await self.pool.fetch(
            """select d::date as day, count(i.id) as n from generate_series(
                   date_trunc('day', now()) - interval '13 days', date_trunc('day', now()), interval '1 day') d
               left join incidents i on date_trunc('day', i.created_at) = d
               group by d order by d""")]
        top_alerts = [dict(r) for r in await self.pool.fetch(
            """select a->>'alertname' as alertname, count(distinct i.id) as n
               from incidents i, jsonb_array_elements(i.alerts) a
               where i.created_at > now() - interval '7 days'
               group by 1 order by 2 desc limit 6""")]
        recent = await self.incidents(limit=8)
        return {
            "by_status": by_status, "opened_24h": opened_24h, "opened_7d": opened_7d,
            "pending_approvals": pending, "mttd": mttd, "mttr": mttr,
            "per_day": per_day, "top_alerts": top_alerts, "recent": recent,
        }

    async def incidents(self, status: str = "open", q: str = "", limit: int = 50, offset: int = 0) -> list[dict]:
        if not await self.has_incidents_table():
            return []
        where, args = [], []
        if status == "open":
            args.append(OPEN_STATUSES)
            where.append(f"status = any(${len(args)}::text[])")
        elif status and status != "all":
            args.append(status)
            where.append(f"status = ${len(args)}")
        if q:
            args.append(f"%{q}%")
            where.append(f"(title ilike ${len(args)} or key ilike ${len(args)} or id::text like ${len(args)})")
        args += [limit, offset]
        sql = (
            "select id, key, status, title, confidence, playbook, created_at, updated_at, resolved_at, "
            "target, jsonb_array_length(alerts) as alert_count from incidents "
            + (f"where {' and '.join(where)} " if where else "")
            + f"order by updated_at desc limit ${len(args) - 1} offset ${len(args)}"
        )
        return [{**dict(r), "id": str(r["id"]), "target": _json(r["target"])} for r in await self.pool.fetch(sql, *args)]

    async def incident(self, incident_id: str) -> dict | None:
        try:
            key = uuid.UUID(incident_id)
        except ValueError:
            return None
        row = await self.pool.fetchrow("select * from incidents where id = $1", key)
        if not row:
            return None
        events = await self.pool.fetch(
            "select ts, type, data from incident_events where incident_id = $1 order by ts", key)
        approvals = await self.pool.fetch(
            "select * from approvals where incident_id = $1 order by requested_at desc", key)
        return {
            **dict(row),
            "id": str(row["id"]),
            "target": _json(row["target"]),
            "alerts": _json(row["alerts"]),
            "decision": _json(row["decision"]),
            "events": [{**dict(e), "data": _json(e["data"])} for e in events],
            "approvals": [{**dict(a), "id": str(a["id"]), "request": _json(a["request"])} for a in approvals],
        }

    async def similar(self, inc: dict, limit: int = 5) -> list[dict]:
        """Diagnosed past incidents like this one (same query the controller uses for its prompt)."""
        query = similar.tsquery(inc["title"], inc.get("alerts") or [], inc.get("target") or {})
        if not query:
            return []
        rows = await self.pool.fetch(similar.SQL, uuid.UUID(inc["id"]), query, limit)
        return [{**dict(r), "id": str(r["id"])} for r in rows]

    async def target_stats(self, days: int = 7) -> list[dict]:
        """Incidents per cluster or host: open now and opened in the last `days` days."""
        if not await self.has_incidents_table():
            return []
        rows = await self.pool.fetch(
            """select coalesce(nullif(target->>'host', ''), target->>'cluster') as name,
                      case when coalesce(target->>'host', '') <> '' then 'host' else 'cluster' end as kind,
                      count(*) filter (where status = any($1::text[])) as open,
                      count(*) filter (where created_at > now() - make_interval(days => $2)) as recent,
                      max(created_at) as last_at
               from incidents
               where coalesce(nullif(target->>'host', ''), target->>'cluster', '') <> ''
               group by 1, 2 order by 1""",
            OPEN_STATUSES, days,
        )
        return [dict(r) for r in rows]

    # Agent registry (table owned by the Tool Hub)

    async def agents(self, stale_after_seconds: int = 180) -> list[dict] | None:
        """Every agent that ever connected; None when the hub has not created the table yet."""
        if not await self.pool.fetchval("select to_regclass('public.agents') is not null"):
            return None
        if await self.pool.fetchval("select to_regclass('public.agent_tokens') is not null"):
            # The credential the agent last connected with: its token or its certificate.
            sql = """select a.*, case when online and last_seen_at > now() - make_interval(secs => $1) then 'online'
                                   when online then 'stale' else 'offline' end as state,
                           extract(day from cert_not_after - now())::int as cert_days_left,
                           case when a.auth = 'token' then t.revoked_at else c.revoked_at end as revoked_at,
                           case when a.auth = 'token' then t.revoked_by else c.revoked_by end as revoked_by,
                           coalesce(c.source, 'external') as cert_source, t.issued_at as token_issued_at
                    from agents a left join agent_certs c on c.serial = a.cert_serial
                         left join agent_tokens t on t.id = a.token_id
                    order by online desc, name"""
        else:
            sql = """select a.*, case when online and last_seen_at > now() - make_interval(secs => $1) then 'online'
                                   when online then 'stale' else 'offline' end as state,
                           extract(day from cert_not_after - now())::int as cert_days_left,
                           c.revoked_at, c.revoked_by, coalesce(c.source, 'external') as cert_source
                    from agents a left join agent_certs c on c.serial = a.cert_serial
                    order by online desc, name"""
        rows = await self.pool.fetch(sql, stale_after_seconds)
        return [{**dict(r), "labels": _json(r["labels"]), "tools": _json(r["tools"])} for r in rows]

    async def agent_tables(self) -> bool:
        """The hub creates the agent tables; enrollment needs a hub with the registry."""
        return bool(await self.pool.fetchval("select to_regclass('public.agent_enrollments') is not null"))

    async def create_enrollment(self, name: str, created_by: str, ttl_hours: int, note: str = "",
                                replace: bool = False) -> dict:
        """Creates a one-time join token for an agent name. Returns the token once.

        The name of a known agent is refused unless `replace`: a token for it lets its holder
        take over that target. Earlier unused tokens for the name expire (a failed run can
        simply ask again)."""
        async with self.pool.acquire() as conn, conn.transaction():
            known = await conn.fetchval("select exists(select 1 from agents where name = $1)", name)
            if known and not replace:
                raise ValueError(f"an agent named {name} already exists")
            token = "aisre-join-" + secrets.token_urlsafe(32)
            expires = datetime.now(timezone.utc) + timedelta(hours=ttl_hours)
            await conn.execute(
                "update agent_enrollments set expires_at = now() where name = $1 and used_at is null", name)
            await conn.execute(
                """insert into agent_enrollments (token_hash, name, note, created_by, expires_at)
                   values ($1, $2, $3, $4, $5)""",
                hashlib.sha256(token.encode()).hexdigest(), name, note or None, created_by, expires)
        return {"name": name, "token": token, "expires_at": expires}

    async def enrollments(self, limit: int = 30) -> list[dict]:
        rows = await self.pool.fetch(
            """select name, note, created_by, created_at, expires_at, used_at, used_from, cert_serial,
                      case when used_at is not null then 'used' when expires_at <= now() then 'expired'
                           else 'pending' end as state
               from agent_enrollments order by created_at desc limit $1""", limit)
        return [dict(r) for r in rows]

    async def revoke_agent(self, name: str, by: str) -> dict | None:
        """Revokes every agent token of the name and the certificate it last connected with;
        the hub drops the session within 30 s and refuses them from then on. Returns what was
        revoked, None when the agent has no credential on record."""
        async with self.pool.acquire() as conn, conn.transaction():
            row = await conn.fetchrow("select cert_serial, cert_not_after from agents where name = $1", name)
            tokens: list[str] = []
            if await conn.fetchval("select to_regclass('public.agent_tokens') is not null"):
                tokens = [r["id"] for r in await conn.fetch(
                    """update agent_tokens set revoked_at = now(), revoked_by = $2
                       where name = $1 and revoked_at is null returning id""", name, by)]
            serial = row["cert_serial"] if row else None
            if not serial and not tokens:
                return None
            if serial:
                await conn.execute(
                    """insert into agent_certs (serial, name, not_after, source, revoked_at, revoked_by)
                       values ($1, $2, $3, 'external', now(), $4)
                       on conflict (serial) do update set revoked_at = coalesce(agent_certs.revoked_at, now()),
                           revoked_by = coalesce(agent_certs.revoked_by, excluded.revoked_by)""",
                    serial, name, row["cert_not_after"], by)
            # Pending join tokens for the name would let it come back.
            await conn.execute(
                "update agent_enrollments set expires_at = now() where name = $1 and used_at is null", name)
            return {"serial": serial, "tokens": tokens}

    # API tokens

    async def create_api_token(self, name: str, scopes: list[str], created_by: str,
                               days: int | None) -> tuple[str, dict]:
        token = "aisre_" + secrets.token_urlsafe(32)
        expires = datetime.now(timezone.utc) + timedelta(days=days) if days else None
        row = await self.pool.fetchrow(
            """insert into api_tokens (id, name, prefix, token_hash, scopes, created_by, expires_at)
               values ($1, $2, $3, $4, $5, $6, $7) returning *""",
            uuid.uuid4(), name, token[:12], hashlib.sha256(token.encode()).hexdigest(), scopes, created_by, expires)
        return token, {**dict(row), "id": str(row["id"])}

    async def api_tokens(self) -> list[dict]:
        rows = await self.pool.fetch(
            "select * from api_tokens order by revoked_at is not null, created_at desc")
        return [{**dict(r), "id": str(r["id"])} for r in rows]

    async def revoke_api_token(self, token_id: str) -> bool:
        return await self.pool.fetchval(
            """with u as (update api_tokens set revoked_at = now() where id = $1 and revoked_at is null returning 1)
               select count(*) from u""", uuid.UUID(token_id)) == 1

    async def api_token(self, token: str) -> dict | None:
        """The active token matching `token`; records its use."""
        row = await self.pool.fetchrow(
            """update api_tokens set last_used_at = now()
               where token_hash = $1 and revoked_at is null and (expires_at is null or expires_at > now())
               returning id, name, scopes, created_by""",
            hashlib.sha256(token.encode()).hexdigest())
        return {**dict(row), "id": str(row["id"])} if row else None

    async def forget_agent(self, name: str) -> bool:
        """Removes an offline agent from the registry; it reappears if it connects again."""
        return await self.pool.fetchval(
            "with d as (delete from agents where name = $1 and not online returning 1) select count(*) from d",
            name) == 1

    async def pending_approvals(self) -> list[dict]:
        if not await self.has_incidents_table():
            return []
        rows = await self.pool.fetch(
            """select a.*, i.title, i.target from approvals a join incidents i on i.id = a.incident_id
               where a.status = 'pending' and a.expires_at > now() order by a.requested_at""")
        return [{**dict(r), "id": str(r["id"]), "incident_id": str(r["incident_id"]),
                 "request": _json(r["request"]), "target": _json(r["target"])} for r in rows]

    async def approval_history(self, limit: int = 30) -> list[dict]:
        if not await self.has_incidents_table():
            return []
        rows = await self.pool.fetch(
            """select a.*, i.title from approvals a join incidents i on i.id = a.incident_id
               where a.status <> 'pending' order by coalesce(a.decided_at, a.requested_at) desc limit $1""", limit)
        return [{**dict(r), "id": str(r["id"]), "incident_id": str(r["incident_id"])} for r in rows]
