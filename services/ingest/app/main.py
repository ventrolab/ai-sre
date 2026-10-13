"""Event Ingest: Alertmanager webhooks, scheduled checks, chat commands, approvals."""

from __future__ import annotations

import logging
import os
from contextlib import asynccontextmanager
from typing import Any

from fastapi import Depends, FastAPI, Header, HTTPException, Response
from prometheus_client import CONTENT_TYPE_LATEST, Counter, generate_latest
from pydantic import BaseModel

from aisre_common import bus, logs
from aisre_common.models import ApprovalDecision, ChatMessage, Event, Target

from .normalize import alert_to_event

log = logging.getLogger("ingest")

NATS_URL = os.environ.get("NATS_URL", "nats://localhost:4222")
DEFAULT_CLUSTER = os.environ.get("CLUSTER_NAME", "local")
INGEST_TOKEN = os.environ.get("INGEST_TOKEN", "")

EVENTS = Counter("aisre_ingest_events_total", "Events published", ["kind", "source"])
DUPLICATES = Counter("aisre_ingest_duplicates_total", "Events dropped as duplicates", ["kind"])


class State:
    nc = None
    js = None


@asynccontextmanager
async def lifespan(_: FastAPI):
    logs.setup("ingest")
    State.nc, State.js = await bus.connect(NATS_URL, "ingest")
    yield
    await State.nc.drain()


app = FastAPI(title="ai-sre ingest", lifespan=lifespan)


def check_token(authorization: str | None = Header(default=None)) -> None:
    if INGEST_TOKEN and authorization != f"Bearer {INGEST_TOKEN}":
        raise HTTPException(status_code=401, detail="invalid token")


async def publish(subject: str, event: Event, dedup_id: str | None = None) -> bool:
    headers = {"Nats-Msg-Id": dedup_id} if dedup_id else None
    ack = await State.js.publish(subject, event.model_dump_json().encode(), headers=headers)
    if ack.duplicate:
        DUPLICATES.labels(event.kind).inc()
        return False
    EVENTS.labels(event.kind, event.source).inc()
    return True


@app.post("/v1/alerts/alertmanager", dependencies=[Depends(check_token)])
async def alertmanager(payload: dict[str, Any]) -> dict[str, int]:
    published = 0
    for alert in payload.get("alerts", []):
        event, dedup_id = alert_to_event(alert, DEFAULT_CLUSTER, payload.get("externalURL", ""))
        if await publish(bus.SUBJECT_ALERT, event, dedup_id):
            published += 1
            log.info(
                "alert %s %s key=%s",
                event.payload["alertname"],
                event.payload["status"],
                event.incident_key,
            )
    return {"received": len(payload.get("alerts", [])), "published": published}


class CheckRequest(BaseModel):
    name: str
    query: str
    target: Target | None = None


@app.post("/v1/checks", dependencies=[Depends(check_token)])
async def check(req: CheckRequest) -> dict[str, str]:
    target = req.target or Target(cluster=DEFAULT_CLUSTER)
    event = Event(
        kind="check",
        source="scheduler",
        target=target,
        incident_key=f"{target.cluster}/check/{req.name}",
        payload={"name": req.name, "query": req.query},
    )
    await publish(bus.SUBJECT_CHECK, event)
    return {"event_id": event.id}


@app.post("/v1/chat", dependencies=[Depends(check_token)])
async def chat(msg: ChatMessage) -> dict[str, str]:
    event = Event(
        kind="chat_command",
        source=f"chat:{msg.ref.adapter}",
        target=Target(cluster=DEFAULT_CLUSTER),
        payload=msg.model_dump(mode="json"),
    )
    await publish(bus.SUBJECT_CHAT, event)
    return {"event_id": event.id}


@app.post("/v1/approvals", dependencies=[Depends(check_token)])
async def approval(decision: ApprovalDecision) -> dict[str, str]:
    event = Event(
        kind="approval",
        source=f"chat:{decision.ref.adapter}",
        payload=decision.model_dump(mode="json"),
    )
    # One decision per approval: a double click must not be applied twice.
    await publish(bus.SUBJECT_APPROVAL, event, dedup_id=f"approval:{decision.approval_id}")
    return {"event_id": event.id}


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    if State.nc is None or not State.nc.is_connected:
        raise HTTPException(status_code=503, detail="nats disconnected")
    return {"status": "ok"}


@app.get("/metrics")
async def metrics() -> Response:
    return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)
