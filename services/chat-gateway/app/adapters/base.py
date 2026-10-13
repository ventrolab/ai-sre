from __future__ import annotations

import abc
from dataclasses import dataclass
from typing import Awaitable, Callable

from aisre_common.models import ApprovalDecision, ChatMessage, Notification


@dataclass
class Inbound:
    """Callbacks an adapter uses to hand user input to the system."""

    message: Callable[[ChatMessage], Awaitable[None]]
    approval: Callable[[ApprovalDecision], Awaitable[None]]


class Adapter(abc.ABC):
    name: str

    def __init__(self, inbound: Inbound):
        self.inbound = inbound

    @abc.abstractmethod
    async def run(self) -> None:
        """Receives inbound messages until cancelled."""

    @abc.abstractmethod
    async def broadcast(self, n: Notification) -> None:
        """Sends a notification to the adapter's default chats."""

    @abc.abstractmethod
    async def reply(self, chat_id: str, n: Notification, reply_to: str | None) -> None:
        """Sends a notification to one chat, as an answer to a message."""
