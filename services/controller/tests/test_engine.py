"""Engine flow with in-memory fakes for storage, HolmesGPT and the guard."""

import asyncio
import uuid
from datetime import datetime, timedelta, timezone

import pytest

from aisre_common.models import ChatRef, Event, Target

from app.db import Approval, Incident, alert_record
from app.engine import Engine


class FakeRepo:
    def __init__(self):
        self.incidents: dict[str, Incident] = {}
        self.approvals: dict[str, Approval] = {}
        self.log_entries = []

    async def find_open(self, key):
        for inc in self.incidents.values():
            if inc.key == key and inc.status not in ("resolved", "closed"):
                return inc
        return None

    async def get(self, incident_id):
        return self.incidents.get(incident_id)

    async def create(self, ev, title, kind="alert"):
        inc = Incident(
            id=str(uuid.uuid4()), key=ev.incident_key, kind=kind, status="new", title=title,
            target=ev.target, alerts=[alert_record(ev)],
        )
        self.incidents[inc.id] = inc
        return inc

    async def upsert_alert(self, incident_id, ev):
        inc = self.incidents[incident_id]
        rec = alert_record(ev)
        for i, a in enumerate(inc.alerts):
            if a["fingerprint"] == rec["fingerprint"]:
                inc.alerts[i] = rec
                return "updated"
        inc.alerts.append(rec)
        return "added"

    async def set_status(self, incident_id, status):
        self.incidents[incident_id].status = status

    async def save_rca(self, incident_id, analysis, root_cause, confidence, playbook):
        inc = self.incidents[incident_id]
        inc.analysis, inc.root_cause, inc.confidence, inc.playbook = analysis, root_cause, confidence, playbook
        inc.status = "diagnosed"

    async def save_decision(self, incident_id, decision):
        self.log_entries.append(("decision", decision))

    async def create_approval(self, incident_id, playbook, request, ttl):
        aid = str(uuid.uuid4())
        self.approvals[aid] = Approval(
            id=aid, incident_id=incident_id, playbook=playbook, request=request, status="pending",
            expires_at=datetime.now(timezone.utc) + timedelta(seconds=ttl),
        )
        return aid

    async def get_approval(self, approval_id):
        return self.approvals.get(approval_id)

    async def decide_approval(self, approval_id, status, by):
        ap = self.approvals[approval_id]
        if ap.status != "pending":
            return False
        ap.status = status
        return True

    async def log(self, *args):
        self.log_entries.append(args)

    async def similar(self, inc, limit=3):
        return []

    async def list_open(self, limit=20):
        return [i for i in self.incidents.values() if i.status not in ("resolved", "closed")]


class FakeHolmes:
    def __init__(self, answer):
        self.answer = answer
        self.calls = []

    async def ask(self, question, system=None, history=None):
        self.calls.append(question)
        return {"analysis": self.answer}


class FakeGuard:
    def __init__(self, decision):
        self.decision = decision
        self.executed = []

    async def playbooks(self, alertname="", target_kind=""):
        return [{"name": "restart-crashlooping-pod", "risk": "low", "description": "d", "enabled": True}]

    async def evaluate(self, request):
        return {"decision": self.decision, "reason": "because", "risk": "low",
                "action": {"primitive": "k8s.pod.delete", "params": {"pod": "crashy-x"}}}

    async def execute(self, request):
        self.executed.append(request)
        return 501, {"status": "not_implemented", "reason": "later"}


ANSWER = 'Crash.\n```json\n{"summary": "s", "root_cause": "bad entrypoint", "confidence": 0.8, "playbook": "restart-crashlooping-pod"}\n```'


def alert_event(status="firing", fingerprint="f1", alertname="KubePodCrashLooping"):
    return Event(
        kind="alert", source="alertmanager", fingerprint=fingerprint, incident_key="c/demo/crashy",
        target=Target(kind="k8s.pod", cluster="c", namespace="demo", workload="crashy", pod="crashy-x"),
        payload={"alertname": alertname, "status": status, "labels": {"alertname": alertname}},
    )


def make_engine(decision):
    sent = []

    async def publish(n):
        sent.append(n)

    repo = FakeRepo()
    guard = FakeGuard(decision)
    engine = Engine(repo, FakeHolmes(ANSWER), guard, publish, max_parallel=2, approval_ttl=600)
    return engine, repo, guard, sent


async def drain(engine):
    while engine.tasks:
        await asyncio.gather(*list(engine.tasks))


@pytest.mark.asyncio
async def test_readonly_flow_proposes_and_resolves():
    engine, repo, guard, sent = make_engine("propose")
    await engine.handle(alert_event())
    await engine.handle(alert_event(fingerprint="f2", alertname="KubeDeploymentReplicasMismatch"))
    await drain(engine)

    assert len(repo.incidents) == 1, "related alerts must share one incident"
    inc = next(iter(repo.incidents.values()))
    assert inc.root_cause == "bad entrypoint"
    assert [n.type for n in sent] == ["incident_opened", "rca", "proposal"]
    assert not guard.executed

    await engine.handle(alert_event(status="resolved"))
    assert inc.status != "resolved", "one alert is still firing"
    await engine.handle(alert_event(status="resolved", fingerprint="f2", alertname="KubeDeploymentReplicasMismatch"))
    assert inc.status == "resolved"
    assert sent[-1].type == "incident_resolved"


@pytest.mark.asyncio
async def test_community_approvals_only_in_console():
    engine, repo, guard, sent = make_engine("approval_required")
    engine.console_url = "https://sre.example.com"
    await engine.handle(alert_event())
    await drain(engine)
    request = sent[-1]
    assert request.type == "approval_request" and not request.actions
    assert "https://sre.example.com/approvals" in request.text
    approval_id = next(iter(repo.approvals))

    def decide(adapter):
        ref = ChatRef(adapter=adapter, chat_id="1", user_name="alice")
        return Event(kind="approval", source=f"chat:{adapter}",
                     payload={"approval_id": approval_id, "decision": "approve", "ref": ref.model_dump()})

    await engine.handle(decide("telegram"))
    await drain(engine)
    assert not guard.executed and repo.approvals[approval_id].status == "pending"
    assert sent[-1].type == "answer" and "enterprise edition" in sent[-1].text

    await engine.handle(decide("web"))
    await drain(engine)
    assert len(guard.executed) == 1 and guard.executed[0]["approved_by"] == "web:alice"


@pytest.mark.asyncio
async def test_approval_flow_executes_once():
    engine, repo, guard, sent = make_engine("approval_required")
    engine.chat_approvals = True
    await engine.handle(alert_event())
    await drain(engine)
    request = sent[-1]
    assert request.type == "approval_request"
    approval_id = request.actions[0].approval_id

    ref = ChatRef(adapter="telegram", chat_id="1", user_name="alice")
    approval = Event(kind="approval", source="chat:telegram",
                     payload={"approval_id": approval_id, "decision": "approve", "ref": ref.model_dump()})
    await engine.handle(approval)
    await drain(engine)
    await engine.handle(approval)
    await drain(engine)

    assert len(guard.executed) == 1
    assert guard.executed[0]["approved_by"] == "telegram:alice"
    assert sent[-1].type == "answer" and "already approved" in sent[-1].text


class TwoStepHolmes:
    """First answer has no JSON; the follow-up in the same conversation has it."""

    def __init__(self):
        self.calls = []

    async def ask(self, question, system=None, history=None):
        self.calls.append(history)
        if history is None:
            return {"analysis": "The container exits: FATAL config not found.", "conversation_history": [{"role": "user"}]}
        return {"analysis": '```json\n{"summary": "s", "root_cause": "missing config", "confidence": 0.7, "playbook": null}\n```'}


@pytest.mark.asyncio
async def test_missing_json_is_requested_in_same_conversation():
    engine, repo, guard, sent = make_engine("propose")
    engine.holmes = TwoStepHolmes()
    await engine.handle(alert_event())
    await drain(engine)
    inc = next(iter(repo.incidents.values()))
    assert inc.root_cause == "missing config"
    assert inc.confidence == 0.3, "follow-up conclusions are capped"
    assert inc.playbook is None
    assert inc.analysis == "The container exits: FATAL config not found."
    assert engine.holmes.calls[1] == [{"role": "user"}]


@pytest.mark.asyncio
async def test_chat_gets_short_report_and_db_keeps_full_analysis():
    answer = ('Step 1 ... step 9, very long.\n```json\n{"summary": "s", "root_cause": "bad entrypoint", '
              '"confidence": 0.8, "playbook": null, "chat_report": "crashy crash-loops\\n**Root cause:** bad entrypoint"}\n```')
    sent = []

    async def publish(n):
        sent.append(n)

    repo = FakeRepo()
    engine = Engine(repo, FakeHolmes(answer), FakeGuard("propose"), publish, max_parallel=2, approval_ttl=600)
    await engine.handle(alert_event())
    await drain(engine)
    rca = next(n for n in sent if n.type == "rca")
    assert rca.text == "Confidence: 80%\n\ncrashy crash-loops\n**Root cause:** bad entrypoint"
    assert next(iter(repo.incidents.values())).analysis == "Step 1 ... step 9, very long."


@pytest.mark.asyncio
async def test_evaluation_incident_is_silent_and_never_remediated():
    engine, repo, guard, sent = make_engine("execute")
    ev = alert_event()
    ev.payload["labels"]["aisre_eval"] = "run1"
    await engine.handle(ev)
    await drain(engine)
    inc = next(iter(repo.incidents.values()))
    assert inc.is_eval and inc.root_cause == "bad entrypoint" and inc.playbook == "restart-crashlooping-pod"
    assert sent == [], "evaluation incidents must not reach chat"
    assert not guard.executed and ("remediation_skipped" in [e[1] for e in repo.log_entries if len(e) > 1])
