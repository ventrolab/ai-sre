import asyncio
from datetime import datetime, timezone

import pytest

from app.auth import map_role
from app.config import Config
from app.content import markdown

from .conftest import add_user, csrf_of, login


def test_unauthenticated_redirects_to_login(make_client):
    client, _, _ = make_client()
    r = client.get("/incidents", follow_redirects=False)
    assert r.status_code == 303 and r.headers["location"].startswith("/login?next=%2Fincidents")


def test_local_login_and_dashboard(make_client):
    client, store, _ = make_client()
    assert login(client, password="wrong").status_code == 401
    r = login(client)
    assert r.status_code == 303
    page = client.get("/")
    assert page.status_code == 200 and "Open incidents" in page.text and "KubePodCrashLooping" in page.text
    assert ("admin", "login", None) in store.audits


def test_post_without_csrf_is_rejected(make_client):
    client, _, _ = make_client()
    r = client.post("/login", data={"username": "admin", "password": "admin-password-123"})
    assert r.status_code == 403


def test_viewer_cannot_approve_operator_can(make_client):
    client, store, services = make_client()
    asyncio.run(add_user(store, "vera", "viewer"))
    asyncio.run(add_user(store, "otto", "operator"))
    login(client, "vera", "user-password-123")
    token = csrf_of(client.get("/approvals").text)
    assert client.post("/approvals/a1/approve", data={"csrf": token}).status_code == 403
    client.post("/logout", data={"csrf": token})
    login(client, "otto", "user-password-123")
    token = csrf_of(client.get("/approvals").text)
    r = client.post("/approvals/a1/approve", data={"csrf": token}, follow_redirects=False)
    assert r.status_code == 303 and services.decisions == [("otto", "a1", "approve")]


def test_ask_sends_question_to_controller(make_client):
    client, store, services = make_client()
    login(client)
    token = csrf_of(client.get("/ask").text)
    client.post("/ask", data={"question": "/why is crashy failing?", "csrf": token})
    username, chat_id, text = services.commands[0]
    assert chat_id.startswith("q:") and text == "why is crashy failing?"


def test_local_auth_disabled(make_client):
    client, store, _ = make_client(AUTH_LOCAL_ENABLED="false", AUTH_OIDC_ENABLED="true",
                                   OIDC_ISSUER_URL="https://idp.example.com/realms/x", OIDC_CLIENT_ID="web")
    page = client.get("/login").text
    assert "Sign in with SSO" in page and 'name="password"' not in page
    token = csrf_of(page) if 'name="csrf"' in page else "t"
    assert client.post("/login", data={"username": "admin", "password": "x", "csrf": token}).status_code in (403, 404)
    assert not store.users, "no local admin is bootstrapped without local auth"


def test_last_admin_is_protected(make_client):
    client, store, _ = make_client()
    login(client)
    admin = asyncio.run(store.user_by_name("admin"))
    token = csrf_of(client.get("/admin/users").text)
    r = client.post(f"/admin/users/{admin.id}", data={"action": "role", "role": "viewer", "csrf": token},
                    follow_redirects=False)
    assert "last+active+administrator" in r.headers["location"]
    assert admin.role == "admin"


def test_viewer_cannot_open_admin(make_client):
    client, store, _ = make_client()
    asyncio.run(add_user(store, "vera", "viewer"))
    login(client, "vera", "user-password-123")
    assert client.get("/admin/users").status_code == 403


@pytest.mark.parametrize("claims,expected", [
    ({"groups": ["/ai-sre-operators", "/other"]}, "operator"),
    ({"groups": ["ai-sre-operators", "ai-sre-admins"]}, "admin"),
    ({"groups": ["unrelated"]}, None),
    ({}, None),
])
def test_role_mapping(monkeypatch, claims, expected):
    monkeypatch.setenv("OIDC_ROLE_MAPPING", "ai-sre-admins=admin,ai-sre-operators=operator")
    assert map_role(Config(), claims) == expected


def test_role_mapping_nested_claim_and_default(monkeypatch):
    monkeypatch.setenv("OIDC_ROLES_CLAIM", "realm_access.roles")
    monkeypatch.setenv("OIDC_ROLE_MAPPING", "sre=operator")
    monkeypatch.setenv("OIDC_DEFAULT_ROLE", "viewer")
    cfg = Config()
    assert map_role(cfg, {"realm_access": {"roles": ["sre"]}}) == "operator"
    assert map_role(cfg, {"realm_access": {"roles": []}}) == "viewer"


def test_config_requires_some_authentication(monkeypatch):
    monkeypatch.setenv("WEB_SESSION_SECRET", "x" * 40)
    monkeypatch.setenv("AUTH_LOCAL_ENABLED", "false")
    monkeypatch.setenv("AUTH_OIDC_ENABLED", "false")
    with pytest.raises(ValueError):
        Config().validate()


def test_markdown_escapes_html():
    out = markdown("**bold** <script>alert(1)</script>")
    assert "<strong>bold</strong>" in out and "<script>" not in out


def test_incident_page_shows_similar_incidents(make_client):
    client, store, _ = make_client()
    now = datetime.now(timezone.utc)
    inc_id, past_id, other_id = "11111111-0000-0000-0000-000000000001", "2222", "3333"
    store.incidents_[inc_id] = {
        "id": inc_id, "key": "k", "status": "diagnosed", "title": "KubePodCrashLooping on crashy", "confidence": 0.9,
        "created_at": now, "updated_at": now, "root_cause": None, "analysis": None, "decision": None,
        "target": {"kind": "k8s.pod"}, "alerts": [], "approvals": [],
        "events": [{"ts": now, "type": "similar_incidents", "data": {"ids": [past_id]}}],
    }
    store.similar_ = [
        {"id": past_id, "title": "Crash loop of crashy", "status": "resolved", "root_cause": "bad config",
         "playbook": "restart-deployment", "decision": "auto", "created_at": now},
        {"id": other_id, "title": "Crash loop of other", "status": "closed", "root_cause": "OOM",
         "playbook": None, "decision": None, "created_at": now},
    ]
    login(client)
    page = client.get(f"/incidents/{inc_id}").text
    assert "Similar incidents" in page and f'href="/incidents/{past_id}"' in page and "bad config" in page
    assert page.count("used in RCA") == 1
    store.similar_ = []
    assert "No diagnosed past incidents" in client.get(f"/incidents/{inc_id}").text


def test_targets_page(make_client):
    client, store, services = make_client()
    login(client)
    assert "not configured" in client.get("/targets").text

    client, store, services = make_client(TOOL_HUB_URL="http://hub:8080")
    login(client)
    assert "not reachable" in client.get("/targets").text

    services.targets = [
        {"name": "prod-eu", "kind": "cluster", "labels": {"env": "prod"}, "version": "v1", "remote": "10.0.0.1:1",
         "connected_at": "2026-10-11T17:11:50.326147088Z",
         "tools": ["k8s_logs", "k8s_get", "action:k8s.pod.delete", "check:k8s.pod.ownerExists"]},
        {"name": "db1", "kind": "host", "tools": ["host_logs"], "connected_at": "2026-10-11T17:00:00Z"},
    ]
    store.target_stats_ = [
        {"name": "prod-eu", "kind": "cluster", "open": 2, "recent": 5, "last_at": None},
        {"name": "gone", "kind": "cluster", "open": 1, "recent": 1, "last_at": datetime.now(timezone.utc)},
        {"name": "old", "kind": "host", "open": 0, "recent": 0, "last_at": None},
    ]
    page = client.get("/targets").text
    assert "2 connected" in page and "env=prod" in page and "k8s_logs, k8s_get" in page
    assert "k8s.pod.delete" in page and "ownerExists" not in page and "read-only" in page
    assert 'href="/incidents?status=open&q=prod-eu">2<' in page
    assert "Targets without an agent" in page and "gone" in page and "old" not in page.split("without an agent")[1]


def test_admin_agents_page(make_client):
    client, store, _ = make_client()
    asyncio.run(add_user(store, "otto", "operator"))
    login(client, "otto", "user-password-123")
    assert client.get("/admin/agents", follow_redirects=False).status_code == 403
    token = csrf_of(client.get("/").text)
    client.post("/logout", data={"csrf": token})

    login(client)
    assert "No agent registry yet" in client.get("/admin/agents").text
    now = datetime.now(timezone.utc)
    base = {"kind": "host", "labels": {"env": "prod"}, "version": "v1", "remote": "10.0.0.5:1", "cert_serial": "ab",
            "cert_not_after": now, "cert_days_left": 10, "connected_at": now, "last_seen_at": now,
            "first_seen_at": now, "disconnected_at": now}
    store.agents_ = [
        {**base, "name": "db1", "state": "online", "tools": ["host_logs", "action:host.unit.restart"]},
        {**base, "name": "old-host", "state": "offline", "tools": ["host_logs"]},
    ]
    page = client.get("/admin/agents").text
    assert "1 online" in page and "1 offline" in page and "env=prod" in page and "10 days" in page
    assert page.count("Forget") == 1 and 'action="/admin/agents/old-host/forget"' in page
    token = csrf_of(page)
    r = client.post("/admin/agents/db1/forget", data={"csrf": token}, follow_redirects=False)
    assert "error" in r.headers["location"]
    client.post("/admin/agents/old-host/forget", data={"csrf": token})
    assert [a["name"] for a in store.agents_] == ["db1"] and ("admin", "agent_forget", "old-host") in store.audits


def _agent(name, state="online", **kw):
    now = datetime.now(timezone.utc)
    return {"name": name, "kind": "host", "state": state, "labels": {}, "tools": ["host_logs"], "version": "v1",
            "remote": "10.0.0.5:1", "cert_serial": f"serial-{name}", "cert_not_after": now, "cert_days_left": 300,
            "connected_at": now, "last_seen_at": now, "first_seen_at": now, "disconnected_at": now,
            "revoked_at": None, **kw}


def test_admin_enrolls_and_revokes_agents(make_client):
    client, store, _ = make_client(TOOL_HUB_PUBLIC_URL="wss://hub.example:8443")
    store.agents_ = [_agent("db1"), _agent("local-cluster", labels={"local": "true"})]
    login(client)
    page = client.get("/admin/agents").text
    assert "managed by the chart" in page and "/admin/agents/local-cluster/revoke" not in page
    token = csrf_of(page)
    page = client.post("/admin/agents/enroll", data={"csrf": token, "name": "Bad Name!", "ttl_hours": 1}).text
    assert "lowercase DNS label" in page
    page = client.post("/admin/agents/enroll", data={"csrf": token, "name": "db1", "ttl_hours": 1}).text
    assert "already exists" in page and "aisre-join-" not in page
    page = client.post("/admin/agents/enroll", data={"csrf": token, "name": "web2", "ttl_hours": 2}).text
    assert "aisre-join-0" in page and "wss://hub.example:8443/v1/tunnel" in page and "sudo -u ai-sre ai-sre-host-agent enroll" in page
    # Token auth through the hub's own TLS port: the agent pins the hub CA.
    assert "auth: token" in page and "tokenFile: /etc/ai-sre/credentials/token" in page and "-ca-sha256" in page
    assert ("admin", "agent_enroll_token", "web2") in store.audits

    client.post("/admin/agents/db1/revoke", data={"csrf": token})
    assert store.agents_[0]["revoked_at"] and ("admin", "agent_revoke", "db1") in store.audits
    page = client.get("/admin/agents").text
    assert "revoked" in page and 'data-confirm="Revoke' not in page  # no second revoke button


def test_audit_details_keep_unicode(make_client):
    client, store, _ = make_client()
    store.audit_log_ = [{"ts": datetime.now(timezone.utc), "username": "admin", "action": "ask", "target": None,
                         "detail": {"question": "что занимает диск <b>"}}]
    login(client)
    page = client.get("/admin/audit").text
    assert "что занимает диск" in page and "\\u0447" not in page and "&lt;b&gt;" in page


def test_install_hint_modes(make_client):
    from app import fleet
    from app.config import Config

    import dataclasses

    def hint(**fields):
        return fleet.install_hint(dataclasses.replace(Config(), **fields), "aisre-join-x")

    # Tokens through an ingress with a public certificate: no CA pin.
    h = hint(tool_hub_public_url="wss://agent.example", tool_hub_auth="token", tool_hub_public_ca="system")
    assert h["auth"] == "token" and h["ca_sha256"] == "" and "-ca-sha256" not in h["command"]
    assert "tls:" not in h["config"] and "hubURL: wss://agent.example/v1/tunnel" in h["config"]
    # Certificates: the pin and the certificate paths.
    h = hint(tool_hub_public_url="wss://hub:8443", tool_hub_auth="mtls", tool_hub_public_ca="system")
    assert h["auth"] == "mtls" and h["ca_sha256"] and "-ca-sha256" in h["command"] and "tls.key" in h["config"]


def test_token_agent_listing_and_revoke(make_client):
    client, store, _ = make_client()
    store.agents_ = [_agent("db1", auth="token", token_id="0123456789abcdef", cert_serial=None, cert_not_after=None,
                            token_issued_at=datetime.now(timezone.utc))]
    login(client)
    page = client.get("/admin/agents").text
    assert ">token<" in page and "01234567" in page and "/admin/agents/db1/revoke" in page
    client.post("/admin/agents/db1/revoke", data={"csrf": csrf_of(page)})
    assert store.agents_[0]["revoked_at"] and ("admin", "agent_revoke", "db1") in store.audits


def test_api_tokens_and_agent_api(make_client):
    client, store, _ = make_client()
    store.agents_ = [_agent("db1")]
    login(client)
    token = csrf_of(client.get("/admin/tokens").text)
    page = client.post("/admin/tokens", data={"csrf": token, "name": "ansible", "scopes": ["agents:enroll"],
                                              "days": 30}).text
    assert "aisre_test0" in page and "shown only once" in page
    client.post("/admin/tokens", data={"csrf": token, "name": "reader", "scopes": ["agents:read"], "days": 0})

    api, astore = client, store
    api.cookies.clear()  # no session: the API takes bearer tokens only
    assert api.get("/api/v1/agents").status_code == 401
    assert api.get("/api/v1/agents", headers={"Authorization": "Bearer nope"}).status_code == 401
    enroll_h = {"Authorization": "Bearer aisre_test0"}
    r = api.get("/api/v1/agents", headers=enroll_h)
    assert r.status_code == 403 and "agents:read" in r.json()["error"]
    r = api.post("/api/v1/agents/enrollments", headers=enroll_h, json={"name": "db2", "ttl_hours": 1})
    assert r.status_code == 201 and r.json()["join_token"].startswith("aisre-join-") and "command" in r.json()
    assert ("token:ansible", "agent_enroll_token", "db2") in astore.audits
    r = api.post("/api/v1/agents/enrollments", headers=enroll_h, json={"name": "db1"})
    assert r.status_code == 409
    r = api.get("/api/v1/agents", headers={"Authorization": "Bearer aisre_test1"})
    assert r.status_code == 200 and r.json()["agents"][0]["name"] == "db1"
    assert api.post("/api/v1/agents/db1/revoke", headers=enroll_h).status_code == 403

    login(client)
    token = csrf_of(client.get("/admin/tokens").text)
    client.post(f"/admin/tokens/{store.tokens_[0]['id']}/revoke", data={"csrf": token})
    client.cookies.clear()
    assert client.post("/api/v1/agents/enrollments", headers=enroll_h, json={"name": "db3"}).status_code == 401


def test_status_api_for_cli(make_client):
    client, store, _ = make_client(LLM_PROVIDER="claude", LLM_FALLBACKS="vllm", APP_VERSION="1.2.3")
    now = datetime.now(timezone.utc)
    store.incidents_ = {"7b0c3c9e-0000-4000-8000-000000000001": {
        "id": "7b0c3c9e-0000-4000-8000-000000000001", "key": "k", "kind": "alert", "status": "diagnosed",
        "title": "DiskSpaceLow on db1", "target": {"host": "db1"}, "alerts": [{"alertname": "DiskSpaceLow"}],
        "root_cause": "logs in /var/log/app", "analysis": "...", "confidence": 0.9, "playbook": None,
        "decision": None, "created_at": now, "updated_at": now, "resolved_at": None, "events": [], "approvals": []}}
    login(client)
    token = csrf_of(client.get("/admin/tokens").text)
    client.post("/admin/tokens", data={"csrf": token, "name": "cli", "scopes": ["status:read"], "days": 0})
    client.post("/admin/tokens", data={"csrf": token, "name": "agents", "scopes": ["agents:read"], "days": 0})
    client.cookies.clear()
    h = {"Authorization": "Bearer aisre_test0"}

    assert client.get("/api/v1/status").status_code == 401
    assert client.get("/api/v1/status", headers={"Authorization": "Bearer aisre_test1"}).status_code == 403
    st = client.get("/api/v1/status", headers=h).json()
    assert st["version"] == "1.2.3" and st["remediation"]["mode"] == "readonly"
    assert st["remediation"]["max_risk"] == "low" and st["llm"] == {"provider": "claude", "fallbacks": ["vllm"]}
    assert st["incidents"]["open"] == 2 and st["pending_approvals"] == 1
    assert {c["name"] for c in st["components"]} >= {"Action Guard", "PostgreSQL"}

    assert client.get("/api/v1/incidents", headers=h).json() == {"incidents": []}
    inc = client.get("/api/v1/incidents/7b0c3c9e-0000-4000-8000-000000000001", headers=h).json()
    assert inc["root_cause"] == "logs in /var/log/app" and inc["alert_count"] == 1
    assert client.get("/api/v1/incidents/nope", headers=h).status_code == 404
    assert client.get("/api/v1/approvals", headers=h).json() == {"pending": [], "history": []}


def test_mobile_layout_assets(make_client):
    client, _, _ = make_client()
    login(client)
    page = client.get("/").text
    # Top bar with the drawer toggle (CSS only) and the table labeling script.
    assert 'id="nav-toggle"' in page and 'for="nav-toggle"' in page and "/static/app.js" in page
    js = client.get("/static/app.js")
    assert js.status_code == 200 and "data-label" in js.text
    css = client.get("/static/style.css").text
    assert "@media (max-width: 720px)" in css and "table.stack" in css


def test_web_extensions_are_loaded(make_client, monkeypatch):
    from aisre_common import extensions

    def register(app, cfg, db):
        @app.get("/ext/hello")
        async def hello():
            return {"edition": "test"}

    class EP:
        name, value = "hello", "test:register"

        def load(self):
            return register

    monkeypatch.setattr(extensions, "entry_points", lambda group: [EP()] if group == extensions.WEB else [])
    client, _, _ = make_client()
    assert client.get("/ext/hello").json() == {"edition": "test"}
