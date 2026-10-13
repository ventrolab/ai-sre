"""SRE Controller: consumes events and drives the incident lifecycle."""

from __future__ import annotations

import asyncio
import logging
import signal

from nats.js.api import AckPolicy, ConsumerConfig, DeliverPolicy
from nats.errors import TimeoutError as NatsTimeout
from prometheus_client import Counter, start_http_server

from aisre_common import bus, extensions, logs
from aisre_common.models import Event, Notification

from .clients import Guard, Holmes
from .config import Config
from .db import Repo
from .engine import Engine

log = logging.getLogger("controller")

EVENTS = Counter("aisre_controller_events_total", "Events consumed", ["kind", "result"])


async def run() -> None:
    logs.setup("controller")
    cfg = Config()
    start_http_server(cfg.metrics_port)

    repo = await Repo.connect(cfg.database_url)
    nc, js = await bus.connect(cfg.nats_url, "controller")

    async def publish(n: Notification) -> None:
        await js.publish(bus.SUBJECT_NOTIFY, n.model_dump_json().encode())

    engine = Engine(
        repo,
        Holmes(cfg.holmes_url, cfg.holmes_model, cfg.holmes_timeout),
        Guard(cfg.guard_url),
        publish,
        cfg.max_parallel,
        cfg.approval_ttl,
        cfg.cluster_name,
        cfg.console_url,
    )
    extensions.load(extensions.CONTROLLER, engine, cfg)
    await engine.resume()

    sub = await js.pull_subscribe(
        "events.>",
        durable="controller",
        stream=bus.EVENTS_STREAM,
        config=ConsumerConfig(
            ack_policy=AckPolicy.EXPLICIT,
            deliver_policy=DeliverPolicy.ALL,
            ack_wait=60,
            max_deliver=5,
        ),
    )

    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, stop.set)

    log.info("consuming events")
    while not stop.is_set():
        try:
            msgs = await sub.fetch(10, timeout=5)
        except NatsTimeout:
            continue
        for msg in msgs:
            try:
                ev = Event.model_validate_json(msg.data)
            except Exception:
                log.exception("dropping malformed event")
                await msg.term()
                continue
            try:
                await engine.handle(ev)
                await msg.ack()
                EVENTS.labels(ev.kind, "ok").inc()
            except Exception:
                log.exception("event %s (%s) failed", ev.id, ev.kind)
                EVENTS.labels(ev.kind, "error").inc()
                await msg.nak(delay=10)

    log.info("shutting down")
    await nc.drain()


def main() -> None:
    asyncio.run(run())


if __name__ == "__main__":
    main()
