"""Authentication: local users (bcrypt) and OIDC (Authlib), sessions and roles."""

from __future__ import annotations

import secrets
import time
from collections import defaultdict
from typing import Any

import bcrypt
from fastapi import HTTPException, Request

from .config import ROLES, Config, role_rank

SESSION_USER = "uid"
SESSION_CSRF = "csrf"


def hash_password(password: str) -> str:
    return bcrypt.hashpw(password.encode(), bcrypt.gensalt()).decode()


def check_password(password: str, hashed: str | None) -> bool:
    if not hashed:
        return False
    try:
        return bcrypt.checkpw(password.encode(), hashed.encode())
    except ValueError:
        return False


def password_problem(password: str) -> str | None:
    if len(password) < 12:
        return "Password must be at least 12 characters."
    return None


def claim(data: dict[str, Any], path: str) -> Any:
    """Reads a claim by a dotted path, e.g. realm_access.roles."""
    value: Any = data
    for part in path.split("."):
        if not isinstance(value, dict):
            return None
        value = value.get(part)
    return value


def map_role(cfg: Config, claims: dict[str, Any]) -> str | None:
    """Highest role among mapped groups/roles, else the default role (None denies)."""
    raw = claim(claims, cfg.oidc_roles_claim) or []
    if isinstance(raw, str):
        raw = [raw]
    # Keycloak group claims may carry full paths ("/ai-sre-admins").
    groups = {str(g).lstrip("/") for g in raw}
    roles = [role for group, role in cfg.oidc_role_mapping.items() if group.lstrip("/") in groups]
    if roles:
        return max(roles, key=role_rank)
    return cfg.oidc_default_role or None


class LoginThrottle:
    """Slows down password guessing: N failures per username per window."""

    def __init__(self, limit: int = 5, window: int = 300):
        self.limit, self.window = limit, window
        self.failures: dict[str, list[float]] = defaultdict(list)

    def blocked(self, key: str) -> bool:
        now = time.monotonic()
        self.failures[key] = [t for t in self.failures[key] if now - t < self.window]
        return len(self.failures[key]) >= self.limit

    def fail(self, key: str) -> None:
        self.failures[key].append(time.monotonic())

    def reset(self, key: str) -> None:
        self.failures.pop(key, None)


def csrf_token(request: Request) -> str:
    token = request.session.get(SESSION_CSRF)
    if not token:
        token = secrets.token_urlsafe(32)
        request.session[SESSION_CSRF] = token
    return token


async def verify_csrf(request: Request) -> None:
    """Every state-changing request carries the session token (form field or header)."""
    expected = request.session.get(SESSION_CSRF)
    got = request.headers.get("X-CSRF-Token")
    if not got:
        form = await request.form()
        got = form.get("csrf")
    if not expected or not got or not secrets.compare_digest(str(expected), str(got)):
        raise HTTPException(status_code=403, detail="CSRF token mismatch")


class NotAuthenticated(Exception):
    pass


def require(role: str):
    """FastAPI dependency: the logged-in, enabled user with at least `role`."""
    assert role in ROLES

    async def dependency(request: Request):
        user = request.state.user
        if user is None:
            raise NotAuthenticated()
        if role_rank(user.role) < role_rank(role):
            raise HTTPException(status_code=403, detail=f"Requires role {role}")
        return user

    return dependency
