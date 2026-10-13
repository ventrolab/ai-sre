"""NATS JetStream helpers: stream layout and connection."""

from __future__ import annotations

import asyncio
import logging

import nats
from nats.js import JetStreamContext
from nats.js.api import RetentionPolicy, StreamConfig
from nats.js.errors import NotFoundError

log = logging.getLogger(__name__)

EVENTS_STREAM = "EVENTS"
NOTIFY_STREAM = "NOTIFY"

SUBJECT_ALERT = "events.alert"
SUBJECT_CHECK = "events.check"
SUBJECT_CHAT = "events.chat"
SUBJECT_APPROVAL = "events.approval"
SUBJECT_NOTIFY = "notify.chat"

STREAMS = [
    StreamConfig(
        name=EVENTS_STREAM,
        subjects=["events.>"],
        retention=RetentionPolicy.LIMITS,
        max_age=7 * 24 * 3600,
        # Alertmanager re-sends the same alert on every repeat_interval;
        # Nats-Msg-Id deduplicates those within the window.
        duplicate_window=15 * 60,
    ),
    StreamConfig(
        name=NOTIFY_STREAM,
        subjects=["notify.>"],
        retention=RetentionPolicy.LIMITS,
        max_age=24 * 3600,
    ),
]


async def connect(url: str, name: str) -> tuple[nats.NATS, JetStreamContext]:
    # Reconnect settings do not cover the first connection, and NATS may
    # still be starting: retry it here.
    for attempt in range(1, 31):
        try:
            nc = await nats.connect(
                url,
                name=name,
                max_reconnect_attempts=-1,
                reconnect_time_wait=2,
                connect_timeout=5,
                error_cb=_log_error,
            )
            break
        except Exception as exc:
            if attempt == 30:
                raise
            log.warning("nats not ready (%r), retry %d/30", exc, attempt)
            await asyncio.sleep(2)
    js = nc.jetstream()
    await ensure_streams(js)
    return nc, js


async def _log_error(exc: Exception) -> None:
    log.warning("nats: %s", exc)


async def ensure_streams(js: JetStreamContext) -> None:
    for cfg in STREAMS:
        try:
            await js.stream_info(cfg.name)
            await js.update_stream(cfg)
        except NotFoundError:
            log.info("creating stream %s", cfg.name)
            await js.add_stream(cfg)
