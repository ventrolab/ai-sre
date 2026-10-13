"""Ventrolab AI SRE web admin."""

from __future__ import annotations

import asyncio
import json
import logging
import uuid
from contextlib import asynccontextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib.parse import urlencode

from fastapi import Depends, FastAPI, Form, HTTPException, Request, Response
from fastapi.responses import HTMLResponse, RedirectResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates
from prometheus_client import CONTENT_TYPE_LATEST, Counter, generate_latest
from starlette.middleware.sessions import SessionMiddleware

from aisre_common import extensions

from . import content, fleet
from .api import ApiError, api_error_handler, build_router
from .auth import (
    SESSION_USER,
    LoginThrottle,
    NotAuthenticated,
    check_password,
    csrf_token,
    hash_password,
    map_role,
    password_problem,
    require,
    verify_csrf,
)
from .clients import Services
from .config import ROLES, Config
from .replies import QUESTION_PREFIX
from .store import Store

log = logging.getLogger("web")
HERE = Path(__file__).parent

LOGINS = Counter("aisre_web_logins_total", "Login attempts", ["method", "result"])

STATUS_LABELS = {
    "new": "New", "investigating": "Investigating", "diagnosed": "Diagnosed",
    "awaiting_approval": "Awaiting approval", "remediating": "Remediating", "verifying": "Verifying",
    "investigation_failed": "Investigation failed", "resolved": "Resolved", "closed": "Closed",
    "remediated": "Remediated", "remediation_failed": "Remediation failed",
}


def duration(seconds: float | None) -> str:
    if seconds is None:
        return "-"
    seconds = int(seconds)
    if seconds < 90:
        return f"{seconds}s"
    if seconds < 5400:
        return f"{seconds // 60}m"
    if seconds < 172800:
        return f"{seconds // 3600}h {seconds % 3600 // 60}m"
    return f"{seconds // 86400}d"


def _parse_ts(value: str | None) -> datetime | None:
    try:
        return datetime.fromisoformat(value) if value else None
    except ValueError:
        return None


def readable_json(value: Any) -> str:
    """JSON for display: non-ASCII text as is (Jinja's tojson escapes it to \\uXXXX).
    Returns a plain string, so autoescaping keeps it safe in HTML text."""
    return json.dumps(value, ensure_ascii=False, default=str)


def ago(ts: datetime | None) -> str:
    if not ts:
        return "-"
    return duration((datetime.now(timezone.utc) - ts).total_seconds()) + " ago"


def create_app(cfg: Config, store: Store | None = None, services: Services | None = None) -> FastAPI:
    cfg.validate()
    state: dict[str, Any] = {"store": store, "services": services or Services(cfg), "nats": None}
    throttle = LoginThrottle()

    @asynccontextmanager
    async def lifespan(_: FastAPI):
        stop = asyncio.Event()
        task = None
        if state["store"] is None:
            from aisre_common import bus, logs

            from .replies import consume

            logs.setup("web")
            state["store"] = await Store.connect(cfg.database_url)
            nc, js = await bus.connect(cfg.nats_url, "web")
            state["nats"] = nc
            task = asyncio.create_task(consume(js, state["store"], stop))
        await bootstrap_admin()
        yield
        stop.set()
        if task:
            await asyncio.wait([task], timeout=6)
        if state["nats"]:
            await state["nats"].drain()

    app = FastAPI(title="ai-sre web", lifespan=lifespan, docs_url=None, redoc_url=None, openapi_url=None)
    templates = Jinja2Templates(directory=str(HERE / "templates"))
    templates.env.filters.update(markdown=content.markdown, duration=duration, ago=ago, tojson=readable_json)
    templates.env.globals.update(cfg=cfg, status_labels=STATUS_LABELS, roles=ROLES)
    app.mount("/static", StaticFiles(directory=str(HERE / "static")), name="static")

    oauth = None
    if cfg.oidc_enabled:
        from authlib.integrations.starlette_client import OAuth

        oauth = OAuth()
        oauth.register(
            name="oidc",
            server_metadata_url=f"{cfg.oidc_issuer}/.well-known/openid-configuration",
            client_id=cfg.oidc_client_id,
            client_secret=cfg.oidc_client_secret or None,
            client_kwargs={"scope": cfg.oidc_scopes, "code_challenge_method": "S256"},
        )

    def db() -> Store:
        return state["store"]

    def svc() -> Services:
        return state["services"]

    async def bootstrap_admin() -> None:
        """First start: create the local admin from WEB_ADMIN_PASSWORD."""
        if cfg.local_auth and cfg.admin_password and await db().count_local_users() == 0:
            await db().create_user(cfg.admin_user, "local", "admin", hash_password(cfg.admin_password))
            await db().audit("system", "bootstrap_admin", cfg.admin_user)
            log.info("created local admin %s", cfg.admin_user)

    def render(request: Request, name: str, status_code: int = 200, **ctx: Any) -> HTMLResponse:
        ctx.update(request=request, user=request.state.user, csrf=csrf_token(request), active=ctx.get("active", ""))
        return templates.TemplateResponse(request, name, ctx, status_code=status_code)

    # Session -> user, on every request. Disabled or deleted users lose the session.
    @app.middleware("http")
    async def load_user(request: Request, call_next):
        request.state.user = None
        uid = request.session.get(SESSION_USER)
        if uid and state["store"] is not None:
            user = await db().user_by_id(uid)
            if user and not user.disabled and (user.source != "local" or cfg.local_auth):
                request.state.user = user
            else:
                request.session.clear()
        return await call_next(request)

    app.add_middleware(
        SessionMiddleware,
        secret_key=cfg.session_secret,
        session_cookie="aisre_session",
        max_age=cfg.session_max_age,
        same_site="lax",
        https_only=cfg.secure_cookies,
    )

    async def system_status() -> dict[str, Any]:
        """Dashboard numbers, remediation mode and component health (page and API)."""
        data, guard, nats_ok = await asyncio.gather(
            db().dashboard(), svc().guard_status(), asyncio.sleep(0, result=bool(state["nats"] and state["nats"].is_connected)))
        try:
            db_ok = await db().ping()
        except Exception:
            db_ok = False
        extra = [{"name": "PostgreSQL", "ok": db_ok, "detail": ""}]
        if state["nats"] is not None:
            extra.append({"name": "NATS", "ok": nats_ok, "detail": ""})
        health = await svc().health(extra)
        open_total = sum(n for s, n in data["by_status"].items() if s not in ("resolved", "closed"))
        return {"data": data, "guard": guard, "health": health, "open_total": open_total}

    app.include_router(build_router(cfg, db, system_status))
    extensions.load(extensions.WEB, app, cfg, db)
    app.add_exception_handler(ApiError, api_error_handler)

    @app.exception_handler(NotAuthenticated)
    async def to_login(request: Request, _: NotAuthenticated):
        target = "/login?" + urlencode({"next": request.url.path})
        if request.headers.get("HX-Request"):
            return Response(status_code=401, headers={"HX-Redirect": target})
        return RedirectResponse(target, status_code=303)

    @app.exception_handler(HTTPException)
    async def http_error(request: Request, exc: HTTPException):
        if request.headers.get("HX-Request") or exc.status_code not in (403, 404):
            return Response(str(exc.detail), status_code=exc.status_code)
        return render(request, "error.html", status_code=exc.status_code, code=exc.status_code, detail=exc.detail)

    def safe_next(target: str | None) -> str:
        return target if target and target.startswith("/") and not target.startswith("//") else "/"

    # Authentication

    @app.get("/login", response_class=HTMLResponse)
    async def login_page(request: Request, next: str = "/", error: str = ""):
        if request.state.user:
            return RedirectResponse(safe_next(next), status_code=303)
        return render(request, "login.html", next=safe_next(next), error=error)

    @app.post("/login")
    async def login(request: Request, username: str = Form(...), password: str = Form(...), next: str = Form("/"),
                    _: None = Depends(verify_csrf)):
        if not cfg.local_auth:
            raise HTTPException(status_code=404, detail="Local login is disabled")
        key = username.strip().lower()
        if throttle.blocked(key):
            LOGINS.labels("local", "throttled").inc()
            return render(request, "login.html", status_code=429, next=safe_next(next),
                          error="Too many failed attempts. Try again in a few minutes.")
        user = await db().user_by_name(username.strip())
        if not user or user.source != "local" or user.disabled or not check_password(password, user.password_hash):
            throttle.fail(key)
            LOGINS.labels("local", "failed").inc()
            await db().audit(username.strip() or "-", "login_failed", None, {"method": "local"})
            return render(request, "login.html", status_code=401, next=safe_next(next),
                          error="Invalid username or password.")
        throttle.reset(key)
        request.session.clear()
        request.session[SESSION_USER] = user.id
        await db().touch_login(user.id)
        await db().audit(user.username, "login", None, {"method": "local"})
        LOGINS.labels("local", "ok").inc()
        return RedirectResponse(safe_next(next), status_code=303)

    @app.get("/auth/oidc/login")
    async def oidc_login(request: Request, next: str = "/"):
        if not oauth:
            raise HTTPException(status_code=404, detail="SSO is not configured")
        request.session["next"] = safe_next(next)
        return await oauth.oidc.authorize_redirect(request, f"{cfg.base_url}/auth/oidc/callback")

    @app.get("/auth/oidc/callback")
    async def oidc_callback(request: Request):
        if not oauth:
            raise HTTPException(status_code=404, detail="SSO is not configured")
        try:
            token = await oauth.oidc.authorize_access_token(request)
        except Exception as exc:
            log.warning("oidc callback failed: %r", exc)
            LOGINS.labels("oidc", "failed").inc()
            return render(request, "login.html", status_code=401, next="/", error="Single sign-on failed.")
        claims = dict(token.get("userinfo") or {})
        username = claims.get("preferred_username") or claims.get("email") or claims.get("sub")
        role = map_role(cfg, claims)
        if not role:
            LOGINS.labels("oidc", "denied").inc()
            await db().audit(username, "login_denied", None, {"method": "oidc", "reason": "no mapped role"})
            return render(request, "login.html", status_code=403, next="/",
                          error=f"{username} has no access to AI SRE. Ask an administrator for a group.")
        user = await db().upsert_oidc_user(username, role, claims.get("name"), claims.get("email"))
        if not user:
            await db().audit(username, "login_denied", None, {"method": "oidc", "reason": "local account exists"})
            return render(request, "login.html", status_code=403, next="/",
                          error=f"A local account named {username} exists; it cannot be used via SSO.")
        if user.disabled:
            await db().audit(username, "login_denied", None, {"method": "oidc", "reason": "disabled"})
            return render(request, "login.html", status_code=403, next="/", error="This account is disabled.")
        target = request.session.get("next", "/")
        request.session.clear()
        request.session[SESSION_USER] = user.id
        request.session["sso"] = True
        await db().audit(user.username, "login", None, {"method": "oidc", "role": role})
        LOGINS.labels("oidc", "ok").inc()
        return RedirectResponse(safe_next(target), status_code=303)

    @app.post("/logout")
    async def logout(request: Request, _: None = Depends(verify_csrf)):
        user = request.state.user
        sso = request.session.get("sso")
        request.session.clear()
        if user:
            await db().audit(user.username, "logout")
        if sso and oauth:
            # Also end the identity provider session.
            meta = await oauth.oidc.load_server_metadata()
            end = meta.get("end_session_endpoint")
            if end:
                query = urlencode({"client_id": cfg.oidc_client_id, "post_logout_redirect_uri": f"{cfg.base_url}/login"})
                return RedirectResponse(f"{end}?{query}", status_code=303)
        return RedirectResponse("/login", status_code=303)

    # Dashboard

    @app.get("/", response_class=HTMLResponse)
    async def dashboard(request: Request, user=Depends(require("viewer"))):
        st = await system_status()
        peak = max([d["n"] for d in st["data"]["per_day"]] or [0]) or 1
        return render(request, "dashboard.html", active="dashboard", data=st["data"], guard=st["guard"],
                      health=st["health"], peak=peak, open_total=st["open_total"])

    # Incidents

    @app.get("/incidents", response_class=HTMLResponse)
    async def incidents(request: Request, status: str = "open", q: str = "", page: int = 1,
                        user=Depends(require("viewer"))):
        page = max(page, 1)
        rows = await db().incidents(status=status, q=q.strip(), limit=50, offset=(page - 1) * 50)
        return render(request, "incidents.html", active="incidents", rows=rows, status=status, q=q, page=page)

    @app.get("/incidents/{incident_id}", response_class=HTMLResponse)
    async def incident(request: Request, incident_id: str, user=Depends(require("viewer"))):
        inc = await db().incident(incident_id)
        if not inc:
            raise HTTPException(status_code=404, detail="Incident not found")
        try:
            similar = await db().similar(inc)
        except Exception as exc:
            log.warning("similar incidents lookup failed: %r", exc)
            similar = []
        # Ids the controller put into the last investigation prompt.
        used = next((set(e["data"].get("ids") or []) for e in reversed(inc["events"])
                     if e["type"] == "similar_incidents" and isinstance(e["data"], dict)), set())
        return render(request, "incident.html", active="incidents", inc=inc, similar=similar, similar_used=used)

    @app.post("/incidents/{incident_id}/reinvestigate")
    async def reinvestigate(request: Request, incident_id: str, user=Depends(require("operator")),
                            _: None = Depends(verify_csrf)):
        inc = await db().incident(incident_id)
        if not inc:
            raise HTTPException(status_code=404, detail="Incident not found")
        await svc().send_command(user.username, f"action:{uuid.uuid4().hex}", f"/reinvestigate {incident_id}")
        await db().audit(user.username, "reinvestigate", incident_id, {"title": inc["title"]})
        return RedirectResponse(f"/incidents/{incident_id}?queued=1", status_code=303)

    # Approvals

    @app.get("/approvals", response_class=HTMLResponse)
    async def approvals(request: Request, user=Depends(require("viewer"))):
        pending, history = await asyncio.gather(db().pending_approvals(), db().approval_history())
        return render(request, "approvals.html", active="approvals", pending=pending, history=history)

    @app.post("/approvals/{approval_id}/{decision}")
    async def decide(request: Request, approval_id: str, decision: str, user=Depends(require("operator")),
                     _: None = Depends(verify_csrf)):
        if decision not in ("approve", "reject"):
            raise HTTPException(status_code=404, detail="Unknown decision")
        await svc().decide(user.username, approval_id, decision)
        await db().audit(user.username, decision, approval_id)
        back = request.headers.get("referer") or "/approvals"
        return RedirectResponse(back if back.startswith(cfg.base_url) or back.startswith("/") else "/approvals",
                                status_code=303)

    # Ask HolmesGPT

    @app.get("/ask", response_class=HTMLResponse)
    async def ask_page(request: Request, user=Depends(require("operator"))):
        return render(request, "ask.html", active="ask", questions=await db().questions(user.username))

    @app.post("/ask")
    async def ask(request: Request, question: str = Form(...), user=Depends(require("operator")),
                  _: None = Depends(verify_csrf)):
        question = question.strip()
        if not question:
            return RedirectResponse("/ask", status_code=303)
        qid = await db().create_question(user.username, question)
        # Plain text is an ad-hoc question for the controller; "/" would be read as a command.
        await svc().send_command(user.username, f"{QUESTION_PREFIX}{qid}", question.lstrip("/"))
        await db().audit(user.username, "ask", qid, {"question": question[:200]})
        return RedirectResponse("/ask", status_code=303)

    @app.get("/ask/{qid}", response_class=HTMLResponse)
    async def ask_item(request: Request, qid: str, user=Depends(require("operator"))):
        item = await db().question(qid, user.username)
        if not item:
            raise HTTPException(status_code=404, detail="Question not found")
        return render(request, "_question.html", q=item)

    # Catalog and knowledge

    @app.get("/targets", response_class=HTMLResponse)
    async def targets(request: Request, user=Depends(require("viewer"))):
        agents, stats = await asyncio.gather(svc().hub_targets(), db().target_stats())
        by_name = {(st["kind"], st["name"]): st for st in stats}
        rows = []
        for a in agents or []:
            tools = a.get("tools") or []
            rows.append({
                **a,
                "read_tools": [t for t in tools if ":" not in t],
                "actions": [t.split(":", 1)[1] for t in tools if t.startswith("action:")],
                "stats": by_name.get((a.get("kind"), a.get("name")), {}),
                "connected_at": _parse_ts(a.get("connected_at")),
            })
        # Targets that had incidents but have no agent now: offline, or never had one.
        connected = {(a.get("kind"), a.get("name")) for a in agents or []}
        missing = [st for key, st in by_name.items() if key not in connected and (st["open"] or st["recent"])]
        return render(request, "targets.html", active="targets", rows=rows, missing=missing,
                      hub_configured=bool(cfg.tool_hub_url), hub_ok=agents is not None)

    @app.get("/playbooks", response_class=HTMLResponse)
    async def playbooks(request: Request, user=Depends(require("viewer"))):
        guard, items = await asyncio.gather(svc().guard_status(), svc().guard_playbooks())
        return render(request, "playbooks.html", active="playbooks", guard=guard, items=items,
                      sources=content.playbook_sources(cfg.playbooks_path))

    @app.get("/knowledge", response_class=HTMLResponse)
    async def knowledge(request: Request, user=Depends(require("viewer"))):
        return render(request, "knowledge.html", active="knowledge", skills=content.skills(cfg.skills_path))

    # Administration

    async def agents_page(request: Request, status_code: int = 200, **extra):
        rows = await db().agents()
        for a in rows or []:
            tools = a.get("tools") or []
            a["read_tools"] = [t for t in tools if ":" not in t]
            a["actions"] = [t.split(":", 1)[1] for t in tools if t.startswith("action:")]
        counts = {s: sum(a["state"] == s for a in rows or []) for s in ("online", "stale", "offline")}
        enroll_ok = rows is not None and await db().agent_tables()
        return render(request, "agents.html", status_code=status_code, active="agents", rows=rows, counts=counts,
                      enroll_ok=enroll_ok, enrollments=await db().enrollments() if enroll_ok else [],
                      ca_sha256=fleet.ca_fingerprint(cfg), hub_url=cfg.tool_hub_public_url, hub_auth=cfg.tool_hub_auth,
                      max_ttl=fleet.MAX_TTL_HOURS, **{"error": request.query_params.get("error", ""), **extra})

    @app.get("/admin/agents", response_class=HTMLResponse)
    async def admin_agents(request: Request, user=Depends(require("admin"))):
        return await agents_page(request)

    @app.post("/admin/agents/enroll", response_class=HTMLResponse)
    async def admin_enroll(request: Request, name: str = Form(...), note: str = Form(""), ttl_hours: int = Form(24),
                           replace: bool = Form(False), user=Depends(require("admin")),
                           _: None = Depends(verify_csrf)):
        name = name.strip().lower()
        if not fleet.valid_name(name):
            return await agents_page(request, 400, error="The name must be a lowercase DNS label "
                                                         "(a-z, 0-9, '.', '-'), at most 63 characters.")
        if not await db().agent_tables():
            return await agents_page(request, 503, error="The agent registry is not available.")
        try:
            e = await db().create_enrollment(name, user.username, max(1, min(ttl_hours, fleet.MAX_TTL_HOURS)),
                                             note.strip(), replace)
        except ValueError as exc:
            return await agents_page(request, 409, error=f"{exc}. Tick 're-enroll' to replace it.")
        await db().audit(user.username, "agent_enroll_token", name, {"ttl_hours": ttl_hours, "replace": replace})
        return await agents_page(request, new_enrollment={**e, **fleet.install_hint(cfg, e["token"])})

    @app.post("/admin/agents/{name}/revoke")
    async def admin_revoke(request: Request, name: str, user=Depends(require("admin")),
                           _: None = Depends(verify_csrf)):
        revoked = await db().revoke_agent(name, user.username)
        if not revoked:
            return RedirectResponse("/admin/agents?" + urlencode({"error": f"{name}: no credential recorded."}),
                                    status_code=303)
        await db().audit(user.username, "agent_revoke", name, revoked)
        return RedirectResponse("/admin/agents", status_code=303)

    @app.get("/admin/tokens", response_class=HTMLResponse)
    async def admin_tokens(request: Request, user=Depends(require("admin"))):
        return render(request, "tokens.html", active="tokens", tokens=await db().api_tokens(), scopes=fleet.SCOPES,
                      error=request.query_params.get("error", ""))

    @app.post("/admin/tokens", response_class=HTMLResponse)
    async def admin_create_token(request: Request, name: str = Form(...), days: int = Form(90),
                                 user=Depends(require("admin")), _: None = Depends(verify_csrf)):
        form = await request.form()
        scopes = [s for s in form.getlist("scopes") if s in fleet.SCOPES]
        name = name.strip()
        if not name or not scopes:
            return RedirectResponse("/admin/tokens?" + urlencode({"error": "A name and at least one scope are required."}),
                                    status_code=303)
        token, row = await db().create_api_token(name, scopes, user.username, days if days > 0 else None)
        await db().audit(user.username, "api_token_create", name, {"scopes": scopes, "days": days})
        return render(request, "tokens.html", active="tokens", tokens=await db().api_tokens(), scopes=fleet.SCOPES,
                      new_token=token, new_token_row=row, error="")

    @app.post("/admin/tokens/{token_id}/revoke")
    async def admin_revoke_token(request: Request, token_id: str, user=Depends(require("admin")),
                                 _: None = Depends(verify_csrf)):
        if await db().revoke_api_token(token_id):
            await db().audit(user.username, "api_token_revoke", token_id)
        return RedirectResponse("/admin/tokens", status_code=303)

    @app.post("/admin/agents/{name}/forget")
    async def forget_agent(request: Request, name: str, user=Depends(require("admin")),
                           _: None = Depends(verify_csrf)):
        if not await db().forget_agent(name):
            return RedirectResponse("/admin/agents?" + urlencode({"error": f"{name} is online or unknown."}),
                                    status_code=303)
        await db().audit(user.username, "agent_forget", name)
        return RedirectResponse("/admin/agents", status_code=303)

    @app.get("/admin/users", response_class=HTMLResponse)
    async def users(request: Request, error: str = "", user=Depends(require("admin"))):
        return render(request, "users.html", active="users", users=await db().list_users(), error=error)

    @app.post("/admin/users")
    async def create_user(request: Request, username: str = Form(...), password: str = Form(...),
                          role: str = Form(...), user=Depends(require("admin")), _: None = Depends(verify_csrf)):
        username = username.strip()
        error = None
        if not cfg.local_auth:
            error = "Local authentication is disabled."
        elif not username or not username.replace("-", "").replace("_", "").replace(".", "").isalnum():
            error = "Username may contain letters, digits, '.', '-' and '_'."
        elif role not in ROLES:
            error = "Unknown role."
        elif await db().user_by_name(username):
            error = f"User {username} already exists."
        else:
            error = password_problem(password)
        if error:
            return RedirectResponse("/admin/users?" + urlencode({"error": error}), status_code=303)
        await db().create_user(username, "local", role, hash_password(password))
        await db().audit(user.username, "user_create", username, {"role": role})
        return RedirectResponse("/admin/users", status_code=303)

    @app.post("/admin/users/{user_id}")
    async def update_user(request: Request, user_id: str, action: str = Form(...), role: str = Form(""),
                          password: str = Form(""), user=Depends(require("admin")), _: None = Depends(verify_csrf)):
        target = await db().user_by_id(user_id)
        if not target:
            raise HTTPException(status_code=404, detail="User not found")

        def back(error: str = "") -> RedirectResponse:
            return RedirectResponse("/admin/users" + (("?" + urlencode({"error": error})) if error else ""),
                                    status_code=303)

        losing_admin = target.role == "admin" and not target.disabled
        if action == "role":
            if target.source != "local":
                return back("Roles of SSO users come from identity provider groups.")
            if role not in ROLES:
                return back("Unknown role.")
            if losing_admin and role != "admin" and await db().count_active_admins() <= 1:
                return back("The last active administrator cannot be demoted.")
            await db().update_user(user_id, role=role)
        elif action in ("disable", "enable"):
            if action == "disable" and target.id == user.id:
                return back("You cannot disable yourself.")
            if action == "disable" and losing_admin and await db().count_active_admins() <= 1:
                return back("The last active administrator cannot be disabled.")
            await db().update_user(user_id, disabled=(action == "disable"))
        elif action == "password":
            if target.source != "local":
                return back("SSO users have no local password.")
            if problem := password_problem(password):
                return back(problem)
            await db().update_user(user_id, password_hash=hash_password(password))
        else:
            return back("Unknown action.")
        await db().audit(user.username, f"user_{action}", target.username, {"role": role} if action == "role" else {})
        return back()

    @app.get("/admin/audit", response_class=HTMLResponse)
    async def audit(request: Request, user=Depends(require("admin"))):
        return render(request, "audit.html", active="audit", entries=await db().audit_log())

    @app.get("/admin/settings", response_class=HTMLResponse)
    async def settings(request: Request, user=Depends(require("admin"))):
        return render(request, "settings.html", active="settings", guard=await svc().guard_status())

    # Service endpoints

    @app.get("/healthz")
    async def healthz():
        return {"status": "ok"}

    @app.get("/metrics")
    async def metrics():
        return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)

    return app


def build() -> FastAPI:
    return create_app(Config())
