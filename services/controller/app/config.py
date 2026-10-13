from __future__ import annotations

import os
from dataclasses import dataclass, field


def _int(name: str, default: int) -> int:
    return int(os.environ.get(name, default))


@dataclass(frozen=True)
class Config:
    nats_url: str = field(default_factory=lambda: os.environ.get("NATS_URL", "nats://localhost:4222"))
    database_url: str = field(default_factory=lambda: os.environ["DATABASE_URL"])
    holmes_url: str = field(default_factory=lambda: os.environ.get("HOLMES_URL", "http://holmes:80"))
    holmes_model: str = field(default_factory=lambda: os.environ.get("HOLMES_MODEL", "default"))
    holmes_timeout: int = field(default_factory=lambda: _int("HOLMES_TIMEOUT_SECONDS", 900))
    guard_url: str = field(default_factory=lambda: os.environ.get("ACTION_GUARD_URL", "http://action-guard:8080"))
    max_parallel: int = field(default_factory=lambda: _int("MAX_PARALLEL_INVESTIGATIONS", 2))
    approval_ttl: int = field(default_factory=lambda: _int("APPROVAL_TTL_SECONDS", 3600))
    cluster_name: str = field(default_factory=lambda: os.environ.get("CLUSTER_NAME", ""))
    console_url: str = field(default_factory=lambda: os.environ.get("WEB_BASE_URL", ""))
    metrics_port: int = field(default_factory=lambda: _int("METRICS_PORT", 9090))
