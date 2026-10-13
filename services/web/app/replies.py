"""Consumes notifications addressed to the web adapter and stores answers."""

from __future__ import annotations

import asyncio
import logging

from nats.errors import TimeoutError as NatsTimeout
from nats.js.api import AckPolicy, ConsumerConfig, DeliverPolicy

from aisre_common import bus
from aisre_common.models import Notification

from .clients import WEB_ADAPTER
from .store import Store

log = logging.getLogger("web.replies")

QUESTION_PREFIX = "q:"


async def consume(js, store: Store, stop: asyncio.Event) -> None:
    sub = await js.pull_subscribe(
        "notify.>",
        durable="web",
        stream=bus.NOTIFY_STREAM,
        config=ConsumerConfig(ack_policy=AckPolicy.EXPLICIT, deliver_policy=DeliverPolicy.NEW, ack_wait=30),
    )
    while not stop.is_set():
        try:
            msgs = await sub.fetch(20, timeout=5)
        except NatsTimeout:
            continue
        except Exception:
            log.exception("fetch failed")
            await asyncio.sleep(2)
            continue
        for msg in msgs:
            try:
                n = Notification.model_validate_json(msg.data)
                ref = n.reply_to
                if ref and ref.adapter == WEB_ADAPTER and ref.chat_id.startswith(QUESTION_PREFIX):
                    qid = ref.chat_id[len(QUESTION_PREFIX):]
                    if n.progress:
                        await store.update_question(qid, "running", None)
                    else:
                        await store.update_question(qid, "done", n.text)
            except Exception:
                log.exception("reply handling failed")
            await msg.ack()
