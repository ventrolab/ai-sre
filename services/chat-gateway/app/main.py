"""Chat Gateway: notifications out to messengers, commands and approvals in."""

from __future__ import annotations

import asyncio
import logging
import os
import signal

import httpx
from nats.errors import TimeoutError as NatsTimeout
from nats.js.api import AckPolicy, ConsumerConfig, DeliverPolicy
from prometheus_client import Counter, start_http_server

from aisre_common import bus, logs
from aisre_common.models import ApprovalDecision, ChatMessage, Notification

from . import adapters
from .adapters.base import Adapter, Inbound

log = logging.getLogger("chat-gateway")

SENT = Counter("aisre_chat_notifications_total", "Notifications delivered", ["adapter", "result"])
INBOUND = Counter("aisre_chat_inbound_total", "Inbound messages", ["adapter", "kind"])


class IngestClient:
    def __init__(self, url: str, token: str):
        headers = {"Authorization": f"Bearer {token}"} if token else {}
        self.http = httpx.AsyncClient(base_url=url, headers=headers, timeout=15)

    async def message(self, msg: ChatMessage) -> None:
        INBOUND.labels(msg.ref.adapter, "message").inc()
        (await self.http.post("/v1/chat", content=msg.model_dump_json())).raise_for_status()

    async def approval(self, decision: ApprovalDecision) -> None:
        INBOUND.labels(decision.ref.adapter, "approval").inc()
        (await self.http.post("/v1/approvals", content=decision.model_dump_json())).raise_for_status()


async def deliver(adapter_list: list[Adapter], n: Notification) -> None:
    for adapter in adapter_list:
        if n.reply_to and n.reply_to.adapter != adapter.name:
            continue
        try:
            if n.reply_to:
                await adapter.reply(n.reply_to.chat_id, n, n.reply_to.message_id)
            else:
                await adapter.broadcast(n)
            SENT.labels(adapter.name, "ok").inc()
        except Exception:
            SENT.labels(adapter.name, "error").inc()
            log.exception("delivery of %s via %s failed", n.id, adapter.name)


async def run() -> None:
    logs.setup("chat-gateway")
    start_http_server(int(os.environ.get("METRICS_PORT", "9090")))

    ingest = IngestClient(os.environ.get("INGEST_URL", "http://ingest:8080"), os.environ.get("INGEST_TOKEN", ""))
    names = [x.strip() for x in os.environ.get("CHAT_ADAPTERS", "console").split(",") if x.strip()]
    adapter_list = adapters.load(names, Inbound(message=ingest.message, approval=ingest.approval))
    log.info("adapters: %s", ", ".join(a.name for a in adapter_list))

    nc, js = await bus.connect(os.environ.get("NATS_URL", "nats://localhost:4222"), "chat-gateway")
    sub = await js.pull_subscribe(
        "notify.>",
        durable="chat-gateway",
        stream=bus.NOTIFY_STREAM,
        config=ConsumerConfig(ack_policy=AckPolicy.EXPLICIT, deliver_policy=DeliverPolicy.NEW, ack_wait=60),
    )

    runners = [asyncio.create_task(a.run()) for a in adapter_list]
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)

    while not stop.is_set():
        try:
            msgs = await sub.fetch(10, timeout=5)
        except NatsTimeout:
            continue
        for msg in msgs:
            try:
                n = Notification.model_validate_json(msg.data)
            except Exception:
                log.exception("dropping malformed notification")
                await msg.term()
                continue
            await deliver(adapter_list, n)
            await msg.ack()

    for r in runners:
        r.cancel()
    await nc.drain()


def main() -> None:
    asyncio.run(run())


if __name__ == "__main__":
    main()
