"""Telegram adapter: Bot API long polling, HTML messages, inline approval buttons."""

from __future__ import annotations

import asyncio
import html
import logging
import os
import re

import httpx

from aisre_common.models import ApprovalDecision, ChatMessage, ChatRef, Notification

from .base import Adapter, Inbound

log = logging.getLogger("chat-gateway.telegram")

MAX_TEXT = 3800  # Telegram limit is 4096 characters after entity parsing.
SEVERITY_MARK = {"critical": "[CRITICAL]", "warning": "[WARNING]", "ok": "[OK]"}


def _ids(value: str) -> set[str]:
    return {x.strip() for x in value.split(",") if x.strip()}


_FENCE = re.compile(r"```[^\n]*\n(.*?)(?:```|$)", re.S)
_INLINE_CODE = re.compile(r"`([^`\n]+)`")
_BOLD = re.compile(r"\*\*(.+?)\*\*")
# Single-asterisk emphasis, rendered bold as in Telegram's own Markdown. The delimiters must
# hug the text and not touch word characters, so "2 * 3", "a*b*c" and "*.yaml" stay as they are.
_EMPHASIS = re.compile(r"(?<![\w*])\*(?=[^\s*])([^*\n]+?)(?<=\S)\*(?![\w*])")
_HEADING = re.compile(r"^#{1,6}\s+(.+)$", re.M)
_LINK = re.compile(r"\[([^\]]+)\]\((https?://[^)\s]+)\)")


def _inline(text: str) -> str:
    """Markdown outside code blocks -> Telegram HTML."""
    codes: list[str] = []

    def keep_code(m: re.Match) -> str:
        codes.append(f"<code>{html.escape(m.group(1))}</code>")
        return f"\x00{len(codes) - 1}\x00"

    text = _INLINE_CODE.sub(keep_code, text)
    text = html.escape(text, quote=False)
    text = _HEADING.sub(r"<b>\1</b>", text)
    text = _BOLD.sub(r"<b>\1</b>", text)
    text = _EMPHASIS.sub(r"<b>\1</b>", text)
    text = _LINK.sub(lambda m: f'<a href="{html.escape(m.group(2))}">{m.group(1)}</a>', text)
    return re.sub(r"\x00(\d+)\x00", lambda m: codes[int(m.group(1))], text)


def markdown_to_html(text: str) -> str:
    out, pos = [], 0
    for m in _FENCE.finditer(text):
        out.append(_inline(text[pos : m.start()]))
        out.append(f"<pre>{html.escape(m.group(1).rstrip())}</pre>")
        pos = m.end()
    out.append(_inline(text[pos:]))
    return "".join(out)


def incident_url(web_url: str, incident_id: str) -> str:
    return f"{web_url.rstrip('/')}/incidents/{incident_id}" if web_url else ""


def render(n: Notification, web_url: str = "") -> str:
    mark = SEVERITY_MARK.get(n.severity, "")
    title = f"<b>{html.escape(n.title)}</b>" if n.title else ""
    if mark:
        title = f"{mark} {title}"
    body = n.text or ""
    if len(body) > MAX_TEXT:
        body = body[:MAX_TEXT] + "\n... (truncated)"
    footer = ""
    if n.incident_id:
        label = f"incident {n.incident_id[:8]}"
        if url := incident_url(web_url, n.incident_id):
            label = f'<a href="{html.escape(url)}">{label}: full report</a>'
        footer = f"\n<i>{label}</i>"
    parts = [p for p in (title, markdown_to_html(body)) if p]
    return "\n\n".join(parts) + footer


def render_plain(n: Notification, web_url: str = "") -> str:
    text = "\n\n".join(p for p in (n.title, n.text[:MAX_TEXT]) if p)
    if not n.incident_id:
        return text
    url = incident_url(web_url, n.incident_id)
    return text + f"\nincident {n.incident_id[:8]}" + (f": {url}" if url else "")


def keyboard(n: Notification) -> dict | None:
    if not n.actions:
        return None
    return {
        "inline_keyboard": [
            [
                {"text": a.label, "callback_data": f"ap:{a.approval_id}:{'a' if a.decision == 'approve' else 'r'}"}
                for a in n.actions
            ]
        ]
    }


class TelegramAdapter(Adapter):
    name = "telegram"

    def __init__(
        self, inbound: Inbound, token: str, chat_ids: set[str], approvers: set[str], thread_id: str = "",
        web_url: str = "",
    ):
        super().__init__(inbound)
        # Web console base URL: incident footers link to the full report there.
        self.web_url = web_url
        self.chat_ids = chat_ids
        self.approvers = approvers
        # Forum supergroups: post into one topic and listen only there.
        self.thread_id = thread_id
        self.http = httpx.AsyncClient(base_url=f"https://api.telegram.org/bot{token}/", timeout=60)
        if not approvers:
            log.warning("TELEGRAM_APPROVERS is empty: nobody can approve actions from Telegram")

    @classmethod
    def from_env(cls, inbound: Inbound) -> "TelegramAdapter":
        token = os.environ.get("TELEGRAM_BOT_TOKEN", "")
        if not token:
            raise ValueError("TELEGRAM_BOT_TOKEN is required for the telegram adapter")
        return cls(
            inbound,
            token,
            _ids(os.environ.get("TELEGRAM_CHAT_IDS", "")),
            _ids(os.environ.get("TELEGRAM_APPROVERS", "")),
            os.environ.get("TELEGRAM_THREAD_ID", "").strip(),
            os.environ.get("WEB_BASE_URL", "").strip(),
        )

    async def call(self, method: str, **params) -> dict:
        resp = await self.http.post(method, json=params)
        data = resp.json()
        if not data.get("ok"):
            raise RuntimeError(f"telegram {method}: {data.get('description')}")
        return data["result"]

    async def send(self, chat_id: str, n: Notification, reply_to: str | None = None) -> None:
        params = {
            "chat_id": chat_id,
            "text": render(n, self.web_url),
            "parse_mode": "HTML",
            "link_preview_options": {"is_disabled": True},
        }
        if markup := keyboard(n):
            params["reply_markup"] = markup
        if self.thread_id:
            params["message_thread_id"] = int(self.thread_id)
        if reply_to:
            params["reply_parameters"] = {"message_id": int(reply_to), "allow_sending_without_reply": True}
        try:
            await self.call("sendMessage", **params)
        except RuntimeError as exc:
            if "parse entities" not in str(exc):
                raise
            # Markup Telegram rejects must not lose the message.
            log.warning("HTML rejected (%s), sending plain text", exc)
            params.pop("parse_mode")
            params["text"] = render_plain(n, self.web_url)
            await self.call("sendMessage", **params)

    async def broadcast(self, n: Notification) -> None:
        for chat_id in self.chat_ids:
            await self.send(chat_id, n)

    async def reply(self, chat_id: str, n: Notification, reply_to: str | None) -> None:
        await self.send(chat_id, n, reply_to)

    async def run(self) -> None:
        offset = 0
        while True:
            try:
                updates = await self.call(
                    "getUpdates", offset=offset, timeout=50, allowed_updates=["message", "callback_query"]
                )
            except Exception as exc:
                log.warning("getUpdates failed: %s", exc)
                await asyncio.sleep(5)
                continue
            for update in updates:
                offset = update["update_id"] + 1
                try:
                    if "message" in update:
                        await self.on_message(update["message"])
                    elif "callback_query" in update:
                        await self.on_callback(update["callback_query"])
                except Exception:
                    log.exception("update %s failed", update.get("update_id"))

    async def on_message(self, m: dict) -> None:
        chat_id = str(m["chat"]["id"])
        text = m.get("text") or ""
        if not text:
            return
        if chat_id not in self.chat_ids:
            # Tells the operator which id to add to TELEGRAM_CHAT_IDS.
            log.info("ignoring message from chat %s (not in TELEGRAM_CHAT_IDS)", chat_id)
            return
        if self.thread_id and str(m.get("message_thread_id", "")) != self.thread_id:
            return
        user = m.get("from") or {}
        ref = ChatRef(
            adapter=self.name,
            chat_id=chat_id,
            message_id=str(m["message_id"]),
            user_id=str(user.get("id", "")),
            user_name=user.get("username") or user.get("first_name"),
        )
        await self.inbound.message(ChatMessage(ref=ref, text=text))

    async def on_callback(self, q: dict) -> None:
        data = q.get("data") or ""
        user = q.get("from") or {}
        user_id = str(user.get("id", ""))
        msg = q.get("message") or {}
        chat_id = str((msg.get("chat") or {}).get("id", ""))
        if not data.startswith("ap:") or chat_id not in self.chat_ids:
            await self.call("answerCallbackQuery", callback_query_id=q["id"])
            return
        if user_id not in self.approvers:
            await self.call(
                "answerCallbackQuery", callback_query_id=q["id"], text="You are not allowed to approve actions.",
                show_alert=True,
            )
            return
        _, approval_id, code = data.split(":", 2)
        decision = "approve" if code == "a" else "reject"
        ref = ChatRef(
            adapter=self.name,
            chat_id=chat_id,
            message_id=str(msg.get("message_id", "")),
            user_id=user_id,
            user_name=user.get("username") or user.get("first_name"),
        )
        await self.inbound.approval(ApprovalDecision(approval_id=approval_id, decision=decision, ref=ref))
        await self.call("answerCallbackQuery", callback_query_id=q["id"], text=f"{decision.capitalize()}d")
        # Buttons are single-use.
        await self.call("editMessageReplyMarkup", chat_id=chat_id, message_id=msg["message_id"])
