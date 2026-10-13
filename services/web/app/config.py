"""Web admin configuration from environment variables."""

from __future__ import annotations

import os
from dataclasses import dataclass, field


def _bool(name: str, default: bool) -> bool:
    return os.environ.get(name, str(default)).strip().lower() in ("1", "true", "yes", "on")


def _list(name: str) -> list[str]:
    return [x.strip() for x in os.environ.get(name, "").split(",") if x.strip()]


def _mapping(name: str) -> dict[str, str]:
    """"group-a=admin,group-b=operator" -> {"group-a": "admin", ...}"""
    out = {}
    for item in _list(name):
        key, _, value = item.partition("=")
        if key and value:
            out[key.strip()] = value.strip()
    return out


@dataclass(frozen=True)
class Config:
    database_url: str = field(default_factory=lambda: os.environ.get("DATABASE_URL", ""))
    nats_url: str = field(default_factory=lambda: os.environ.get("NATS_URL", "nats://localhost:4222"))
    ingest_url: str = field(default_factory=lambda: os.environ.get("INGEST_URL", "http://ingest:8080"))
    ingest_token: str = field(default_factory=lambda: os.environ.get("INGEST_TOKEN", ""))
    guard_url: str = field(default_factory=lambda: os.environ.get("ACTION_GUARD_URL", "http://action-guard:8080"))
    holmes_url: str = field(default_factory=lambda: os.environ.get("HOLMES_URL", "http://holmes:80"))
    litellm_url: str = field(default_factory=lambda: os.environ.get("LITELLM_URL", "http://litellm:4000"))
    # Optional: the Targets page lists the agents connected to the Tool Hub (read token only).
    tool_hub_url: str = field(default_factory=lambda: os.environ.get("TOOL_HUB_URL", "").rstrip("/"))
    tool_hub_token: str = field(default_factory=lambda: os.environ.get("TOOL_HUB_TOKEN", ""))
    # Agent enrollment: the address agents dial (wss://host:port) and the hub CA certificate
    # (public part only), whose SHA-256 agents pin.
    tool_hub_public_url: str = field(default_factory=lambda: os.environ.get("TOOL_HUB_PUBLIC_URL", "").rstrip("/"))
    tool_hub_ca_file: str = field(default_factory=lambda: os.environ.get("TOOL_HUB_CA_FILE", ""))
    # Tunnel authentication of the hub (token, mtls, both) and whether publicUrl presents the
    # hub CA (hub: agents pin it) or a publicly trusted certificate (system: an ingress).
    tool_hub_auth: str = field(default_factory=lambda: os.environ.get("TOOL_HUB_AUTH", "token"))
    tool_hub_public_ca: str = field(default_factory=lambda: os.environ.get("TOOL_HUB_PUBLIC_CA", "hub"))

    base_url: str = field(default_factory=lambda: os.environ.get("WEB_BASE_URL", "http://localhost:8080").rstrip("/"))
    session_secret: str = field(default_factory=lambda: os.environ.get("WEB_SESSION_SECRET", ""))
    session_max_age: int = field(default_factory=lambda: int(os.environ.get("WEB_SESSION_MAX_AGE", "43200")))
    secure_cookies: bool = field(default_factory=lambda: _bool("WEB_SECURE_COOKIES", False))

    local_auth: bool = field(default_factory=lambda: _bool("AUTH_LOCAL_ENABLED", True))
    admin_user: str = field(default_factory=lambda: os.environ.get("WEB_ADMIN_USER", "admin"))
    admin_password: str = field(default_factory=lambda: os.environ.get("WEB_ADMIN_PASSWORD", ""))

    oidc_enabled: bool = field(default_factory=lambda: _bool("AUTH_OIDC_ENABLED", False))
    oidc_name: str = field(default_factory=lambda: os.environ.get("OIDC_DISPLAY_NAME", "SSO"))
    oidc_issuer: str = field(default_factory=lambda: os.environ.get("OIDC_ISSUER_URL", "").rstrip("/"))
    oidc_client_id: str = field(default_factory=lambda: os.environ.get("OIDC_CLIENT_ID", ""))
    oidc_client_secret: str = field(default_factory=lambda: os.environ.get("OIDC_CLIENT_SECRET", ""))
    oidc_scopes: str = field(default_factory=lambda: os.environ.get("OIDC_SCOPES", "openid profile email"))
    # Claim with groups or roles; dots walk nested objects (realm_access.roles).
    oidc_roles_claim: str = field(default_factory=lambda: os.environ.get("OIDC_ROLES_CLAIM", "groups"))
    oidc_role_mapping: dict[str, str] = field(default_factory=lambda: _mapping("OIDC_ROLE_MAPPING"))
    # Role for users without a mapped group; empty denies access.
    oidc_default_role: str = field(default_factory=lambda: os.environ.get("OIDC_DEFAULT_ROLE", ""))

    playbooks_path: str = field(default_factory=lambda: os.environ.get("PLAYBOOKS_PATH", "/etc/ai-sre/playbooks"))
    skills_path: str = field(default_factory=lambda: os.environ.get("SKILLS_PATH", "/etc/ai-sre/skills"))

    cluster_name: str = field(default_factory=lambda: os.environ.get("CLUSTER_NAME", "local"))
    llm_provider: str = field(default_factory=lambda: os.environ.get("LLM_PROVIDER", ""))
    llm_fallbacks: list[str] = field(default_factory=lambda: _list("LLM_FALLBACKS"))
    chat_adapters: list[str] = field(default_factory=lambda: _list("CHAT_ADAPTERS"))
    version: str = field(default_factory=lambda: os.environ.get("APP_VERSION", "dev"))

    def validate(self) -> None:
        if not self.local_auth and not self.oidc_enabled:
            raise ValueError("enable local authentication, OIDC, or both")
        if self.oidc_enabled and not (self.oidc_issuer and self.oidc_client_id):
            raise ValueError("OIDC needs OIDC_ISSUER_URL and OIDC_CLIENT_ID")
        if len(self.session_secret) < 32:
            raise ValueError("WEB_SESSION_SECRET must be at least 32 characters")
        for role in self.oidc_role_mapping.values():
            if role not in ROLES:
                raise ValueError(f"OIDC_ROLE_MAPPING: unknown role {role!r}")
        if self.oidc_default_role and self.oidc_default_role not in ROLES:
            raise ValueError(f"OIDC_DEFAULT_ROLE: unknown role {self.oidc_default_role!r}")


# Ordered by privilege.
ROLES = ("viewer", "operator", "admin")


def role_rank(role: str) -> int:
    return ROLES.index(role) if role in ROLES else -1
