import pytest

from aisre_common.models import Notification, NotificationAction

from app.adapters.base import Inbound
from app.adapters.telegram import TelegramAdapter, keyboard, markdown_to_html, render, render_plain


def test_render_escapes_html_and_truncates():
    n = Notification(type="rca", title="RCA <pod>", text="a < b & c" + "x" * 5000, incident_id="1234567890", severity="warning")
    out = render(n)
    assert "<b>RCA &lt;pod&gt;</b>" in out
    assert "a &lt; b &amp; c" in out
    assert "(truncated)" in out and len(out) < 4096
    assert out.startswith("[WARNING]")


def test_keyboard_callback_data_fits_limit():
    n = Notification(
        type="approval_request",
        title="t",
        actions=[
            NotificationAction(label="Approve", decision="approve", approval_id="0" * 36),
            NotificationAction(label="Reject", decision="reject", approval_id="0" * 36),
        ],
    )
    buttons = keyboard(n)["inline_keyboard"][0]
    assert [b["callback_data"][-1] for b in buttons] == ["a", "r"]
    assert all(len(b["callback_data"].encode()) <= 64 for b in buttons)


class Recorder:
    def __init__(self):
        self.messages, self.approvals, self.calls = [], [], []


@pytest.fixture
def adapter():
    rec = Recorder()

    async def message(m):
        rec.messages.append(m)

    async def approval(a):
        rec.approvals.append(a)

    a = TelegramAdapter(Inbound(message, approval), "token", {"100"}, {"7"})

    async def call(method, **params):
        rec.calls.append((method, params))
        return {}

    a.call = call
    return a, rec


@pytest.mark.asyncio
async def test_messages_from_unknown_chats_are_ignored(adapter):
    a, rec = adapter
    await a.on_message({"chat": {"id": 999}, "message_id": 1, "text": "hi", "from": {"id": 7}})
    await a.on_message({"chat": {"id": 100}, "message_id": 2, "text": "/status", "from": {"id": 7, "username": "bob"}})
    assert [m.text for m in rec.messages] == ["/status"]
    assert rec.messages[0].ref.user_name == "bob"


@pytest.mark.asyncio
async def test_only_approvers_can_approve(adapter):
    a, rec = adapter
    base = {"id": "q", "data": "ap:abc:a", "message": {"message_id": 5, "chat": {"id": 100}}}
    await a.on_callback({**base, "from": {"id": 8}})
    assert not rec.approvals
    await a.on_callback({**base, "from": {"id": 7, "username": "alice"}})
    assert rec.approvals[0].decision == "approve" and rec.approvals[0].approval_id == "abc"
    assert ("editMessageReplyMarkup", {"chat_id": "100", "message_id": 5}) in rec.calls


@pytest.mark.asyncio
async def test_forum_topic_filter_and_send(adapter):
    a, rec = adapter
    a.thread_id = "101"
    await a.on_message({"chat": {"id": 100}, "message_id": 1, "message_thread_id": 5, "text": "/status", "from": {"id": 7}})
    await a.on_message({"chat": {"id": 100}, "message_id": 2, "message_thread_id": 101, "text": "/mode", "from": {"id": 7}})
    assert [m.text for m in rec.messages] == ["/mode"]
    await a.broadcast(Notification(type="rca", title="t"))
    method, params = rec.calls[-1]
    assert method == "sendMessage" and params["message_thread_id"] == 101


def test_markdown_to_html():
    md = "### Evidence\n**Root cause:** `a<b>` & x\n```\nFATAL: <missing>\n```\nsee [docs](https://example.com/x)"
    out = markdown_to_html(md)
    assert "<b>Evidence</b>" in out
    assert "<b>Root cause:</b> <code>a&lt;b&gt;</code> &amp; x" in out
    assert "<pre>FATAL: &lt;missing&gt;</pre>" in out
    assert '<a href="https://example.com/x">docs</a>' in out


def test_unclosed_fence_is_closed():
    assert markdown_to_html("x\n```\ncode") == "x\n<pre>code</pre>"


def test_single_asterisk_emphasis_is_bold():
    out = markdown_to_html("1. *Resolve current pods*: gone\n2. *Logs, no filter*:")
    assert "1. <b>Resolve current pods</b>: gone" in out and "2. <b>Logs, no filter</b>:" in out
    for plain in ("2 * 3 * 4", "a*b*c", "files *.yaml and *.json", "* item", "**"):
        assert markdown_to_html(plain) == plain
    assert markdown_to_html("`*x*` *y*") == "<code>*x*</code> <b>y</b>"


def test_incident_footer_links_to_web_console():
    n = Notification(type="rca", title="RCA", text="short", incident_id="1234567890abcdef")
    assert render(n).endswith("<i>incident 12345678</i>")
    out = render(n, "https://sre.example.com/")
    assert out.endswith('<i><a href="https://sre.example.com/incidents/1234567890abcdef">incident 12345678: full report</a></i>')
    assert render_plain(n, "https://sre.example.com").endswith(
        "incident 12345678: https://sre.example.com/incidents/1234567890abcdef")
