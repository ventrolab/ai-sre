import os
import re
import uuid
from datetime import datetime, timezone

import pytest

from app.auth import hash_password
from app.config import Config
from app.store import User


class FakeStore:
    def __init__(self):
        self.users: dict[str, User] = {}
        self.audits = []
        self.questions_ = {}
        self.incidents_ = {}
        self.similar_ = []
        self.target_stats_ = []
        self.agents_ = None
        self.enrollments_ = []
        self.tokens_ = []

    async def ping(self):
        return True

    async def user_by_name(self, username):
        return next((u for u in self.users.values() if u.username == username), None)

    async def user_by_id(self, user_id):
        return self.users.get(user_id)

    async def list_users(self):
        return list(self.users.values())

    async def count_local_users(self):
        return sum(u.source == "local" for u in self.users.values())

    async def create_user(self, username, source, role, password_hash=None, display_name=None, email=None):
        u = User(str(uuid.uuid4()), username, display_name, email, source, password_hash, role, False)
        self.users[u.id] = u
        return u

    async def upsert_oidc_user(self, username, role, display_name, email):
        u = await self.user_by_name(username)
        if u and u.source != "oidc":
            return None
        if not u:
            u = await self.create_user(username, "oidc", role, None, display_name, email)
        u.role = role
        return u

    async def touch_login(self, user_id):
        pass

    async def update_user(self, user_id, **fields):
        for k, v in fields.items():
            setattr(self.users[user_id], k, v)

    async def count_active_admins(self):
        return sum(u.role == "admin" and not u.disabled for u in self.users.values())

    async def audit(self, username, action, target=None, detail=None):
        self.audits.append((username, action, target))

    async def audit_log(self, limit=200):
        return getattr(self, "audit_log_", [])

    async def dashboard(self):
        return {"by_status": {"diagnosed": 2, "resolved": 1}, "opened_24h": 1, "opened_7d": 3,
                "pending_approvals": 1, "mttd": 42.0, "mttr": None,
                "per_day": [{"day": datetime(2026, 10, 11).date(), "n": 2}],
                "top_alerts": [{"alertname": "KubePodCrashLooping", "n": 2}], "recent": []}

    async def incidents(self, **kw):
        return []

    async def incident(self, incident_id):
        return self.incidents_.get(incident_id)

    async def similar(self, inc, limit=5):
        return self.similar_

    async def pending_approvals(self):
        return []

    async def target_stats(self, days=7):
        return self.target_stats_

    async def agents(self, stale_after_seconds=180):
        return self.agents_

    async def agent_tables(self):
        return self.agents_ is not None

    async def create_enrollment(self, name, created_by, ttl_hours, note="", replace=False):
        if any(a["name"] == name for a in self.agents_ or []) and not replace:
            raise ValueError(f"an agent named {name} already exists")
        for old in self.enrollments_:
            if old["name"] == name and old["state"] == "pending":
                old["state"] = "expired"
        token = f"aisre-join-{len(self.enrollments_)}"
        e = {"name": name, "token": token, "expires_at": datetime.now(timezone.utc), "state": "pending",
             "note": note, "created_by": created_by, "created_at": datetime.now(timezone.utc), "used_at": None}
        self.enrollments_.append(e)
        return e

    async def enrollments(self, limit=30):
        return self.enrollments_

    async def revoke_agent(self, name, by):
        for a in self.agents_ or []:
            if a["name"] == name and (a.get("cert_serial") or a.get("token_id")):
                a["revoked_at"], a["revoked_by"] = datetime.now(timezone.utc), by
                return {"serial": a.get("cert_serial"), "tokens": [a["token_id"]] if a.get("token_id") else []}
        return None

    async def create_api_token(self, name, scopes, created_by, days):
        token = f"aisre_test{len(self.tokens_)}"
        row = {"id": str(uuid.uuid4()), "name": name, "prefix": token[:12], "scopes": scopes,
               "created_by": created_by, "created_at": datetime.now(timezone.utc), "expires_at": None,
               "last_used_at": None, "revoked_at": None, "_token": token}
        self.tokens_.append(row)
        return token, row

    async def api_tokens(self):
        return self.tokens_

    async def revoke_api_token(self, token_id):
        for t in self.tokens_:
            if t["id"] == token_id and not t["revoked_at"]:
                t["revoked_at"] = datetime.now(timezone.utc)
                return True
        return False

    async def api_token(self, token):
        t = next((t for t in self.tokens_ if t["_token"] == token and not t["revoked_at"]), None)
        return {"id": t["id"], "name": t["name"], "scopes": t["scopes"], "created_by": t["created_by"]} if t else None

    async def forget_agent(self, name):
        before = len(self.agents_ or [])
        self.agents_ = [a for a in self.agents_ or [] if not (a["name"] == name and a["state"] == "offline")]
        return len(self.agents_) < before

    async def approval_history(self, limit=30):
        return []

    async def create_question(self, username, question):
        qid = str(uuid.uuid4())
        self.questions_[qid] = {"id": qid, "username": username, "question": question, "status": "queued",
                                "answer": None, "created_at": datetime.now(timezone.utc)}
        return qid

    async def questions(self, username, limit=20):
        return [q for q in self.questions_.values() if q["username"] == username]

    async def question(self, qid, username):
        q = self.questions_.get(qid)
        return q if q and q["username"] == username else None


class FakeServices:
    def __init__(self):
        self.commands, self.decisions = [], []
        self.targets = None

    async def send_command(self, username, chat_id, text):
        self.commands.append((username, chat_id, text))

    async def decide(self, username, approval_id, decision):
        self.decisions.append((username, approval_id, decision))

    async def guard_status(self):
        return {"mode": "readonly", "autofix_max_risk": "low", "autofix_max_actions_per_hour": 5,
                "catalog_version": "abc", "catalog_valid": True, "playbooks": 8}

    async def guard_playbooks(self):
        return []

    async def hub_targets(self):
        return self.targets

    async def health(self, extra):
        return [{"name": "Action Guard", "ok": True, "detail": ""}] + extra


@pytest.fixture
def make_client(monkeypatch):
    from fastapi.testclient import TestClient

    from app.main import create_app

    def factory(**env):
        base = {"WEB_SESSION_SECRET": "x" * 40, "WEB_ADMIN_PASSWORD": "admin-password-123",
                "AUTH_LOCAL_ENABLED": "true", "AUTH_OIDC_ENABLED": "false"}
        base.update(env)
        for k, v in base.items():
            monkeypatch.setenv(k, v)
        store, services = FakeStore(), FakeServices()
        client = TestClient(create_app(Config(), store, services))
        client.__enter__()
        return client, store, services

    return factory


def csrf_of(html: str) -> str:
    return re.search(r'name="csrf" value="([^"]+)"', html).group(1)


def login(client, username="admin", password="admin-password-123"):
    token = csrf_of(client.get("/login").text)
    return client.post("/login", data={"username": username, "password": password, "next": "/", "csrf": token},
                       follow_redirects=False)


async def add_user(store, username, role, password="user-password-123"):
    return await store.create_user(username, "local", role, hash_password(password))
