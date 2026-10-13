"""Wire models shared by all services.

Events flow from ingest to the controller (stream EVENTS, subjects events.*).
Notifications flow from the controller to the chat gateway (stream NOTIFY,
subjects notify.*).
"""

from __future__ import annotations

import uuid
from datetime import datetime, timezone
from typing import Any, Literal

from pydantic import BaseModel, Field


def utcnow() -> datetime:
    return datetime.now(timezone.utc)


def new_id(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex}"


class Target(BaseModel):
    """What an event is about. Fields are optional because sources differ."""

    kind: str = "unknown"  # k8s.pod, k8s.deployment, k8s.node, host, host.unit, ...
    cluster: str | None = None
    namespace: str | None = None
    workload: str | None = None
    pod: str | None = None
    node: str | None = None
    host: str | None = None
    labels: dict[str, str] = Field(default_factory=dict)

    def describe(self) -> str:
        parts = [self.kind]
        for name in ("cluster", "namespace", "workload", "pod", "node", "host"):
            value = getattr(self, name)
            if value:
                parts.append(f"{name}={value}")
        return " ".join(parts)


EventKind = Literal["alert", "check", "chat_command", "approval"]


class Event(BaseModel):
    id: str = Field(default_factory=lambda: new_id("evt"))
    kind: EventKind
    source: str
    target: Target = Field(default_factory=Target)
    fingerprint: str | None = None
    incident_key: str | None = None
    payload: dict[str, Any] = Field(default_factory=dict)
    received_at: datetime = Field(default_factory=utcnow)


class ChatRef(BaseModel):
    """Where a message came from, so the answer goes to the same place."""

    adapter: str
    chat_id: str
    message_id: str | None = None
    user_id: str | None = None
    user_name: str | None = None


class ChatMessage(BaseModel):
    """Inbound message from a messenger, normalized by the chat gateway."""

    ref: ChatRef
    text: str


class ApprovalDecision(BaseModel):
    approval_id: str
    decision: Literal["approve", "reject"]
    ref: ChatRef


class NotificationAction(BaseModel):
    """A button attached to a notification."""

    label: str
    decision: Literal["approve", "reject"]
    approval_id: str


NotificationType = Literal[
    "incident_opened",
    "rca",
    "proposal",
    "approval_request",
    "action_result",
    "incident_resolved",
    "answer",
    "system",
]


class Notification(BaseModel):
    id: str = Field(default_factory=lambda: new_id("ntf"))
    type: NotificationType
    title: str
    text: str = ""
    severity: str = "info"
    incident_id: str | None = None
    actions: list[NotificationAction] = Field(default_factory=list)
    reply_to: ChatRef | None = None
    # Interim answer ("investigating..."); the final answer follows.
    progress: bool = False
    created_at: datetime = Field(default_factory=utcnow)
