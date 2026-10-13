"""Writes notifications to the log. For tests and installations without a messenger."""

from __future__ import annotations

import asyncio
import logging

from aisre_common.models import Notification

from .base import Adapter

log = logging.getLogger("chat-gateway.console")


class ConsoleAdapter(Adapter):
    name = "console"

    async def run(self) -> None:
        await asyncio.Event().wait()

    async def broadcast(self, n: Notification) -> None:
        log.info("[%s] %s | %s", n.type, n.title, n.text.replace("\n", " / ")[:2000])

    async def reply(self, chat_id: str, n: Notification, reply_to: str | None) -> None:
        await self.broadcast(n)
