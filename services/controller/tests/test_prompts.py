from aisre_common.models import Target

from app.db import Incident
from app.prompts import investigation_prompt, parse_check, parse_investigation


def test_parse_fenced_json():
    text = """<think>let me look</think>
The pod crashes because the image entrypoint exits with code 1.

```json
{"summary": "crash loop", "root_cause": "entrypoint exits 1", "confidence": 0.9,
 "playbook": "restart-crashlooping-pod", "playbook_reason": "transient"}
```"""
    inv = parse_investigation(text, {"restart-crashlooping-pod"})
    assert inv.structured
    assert inv.root_cause == "entrypoint exits 1"
    assert inv.playbook == "restart-crashlooping-pod"
    assert inv.confidence == 0.9
    assert "```" not in inv.analysis and "<think>" not in inv.analysis
    assert inv.analysis.startswith("The pod crashes")


def test_playbook_must_be_offered():
    text = '{"summary": "s", "root_cause": "r", "confidence": 2, "playbook": "rm-rf"}'
    inv = parse_investigation(text, {"restart-crashlooping-pod"})
    assert inv.playbook is None
    assert inv.confidence == 1.0


def test_unstructured_answer_is_kept():
    inv = parse_investigation("I could not reach the cluster.", set())
    assert not inv.structured
    assert inv.analysis == "I could not reach the cluster."


def test_bare_json_with_nested_braces():
    text = 'Analysis {not json}.\n{"root_cause": "x", "playbook": null, "extra": {"a": 1}}'
    inv = parse_investigation(text, set())
    assert inv.structured and inv.root_cause == "x" and inv.playbook is None


def test_parse_check():
    assert parse_check('All good.\n```json\n{"healthy": true, "summary": "ok"}\n```') == (True, "ok")
    assert parse_check("no json")[0] is None


def test_prompt_lists_playbooks_and_alerts():
    inc = Incident(
        id="1",
        key="k",
        kind="alert",
        status="new",
        title="t",
        target=Target(kind="k8s.pod", namespace="demo", pod="crashy-x"),
        alerts=[{"alertname": "KubePodCrashLooping", "status": "firing", "annotations": {"summary": "crash"}}],
    )
    prompt = investigation_prompt(inc, [{"name": "restart-crashlooping-pod", "risk": "low", "description": "d"}])
    assert "KubePodCrashLooping" in prompt and "restart-crashlooping-pod" in prompt and "crash" in prompt


def test_remote_cluster_prompt_routes_to_tool_hub():
    inc = Incident(id="1", key="k", kind="alert", status="new", title="t",
                   target=Target(kind="k8s.pod", cluster="edge", namespace="shop", pod="checkout-x"), alerts=[])
    prompt = investigation_prompt(inc, [], local_cluster="testbed")
    assert "target=edge" in prompt and "not in the local cluster testbed" in prompt
    assert "target=" not in investigation_prompt(inc, [], local_cluster="edge")


def test_similar_incidents_in_prompt():
    from datetime import datetime, timezone
    inc = Incident(id="1", key="k", kind="alert", status="new", title="t", target=Target(kind="k8s.pod"), alerts=[])
    prompt = investigation_prompt(inc, [], similar=[{"title": "KubePodCrashLooping on crashy", "status": "resolved",
        "root_cause": "missing config", "playbook": None, "created_at": datetime(2026, 10, 1, tzinfo=timezone.utc)}])
    assert "Similar past incidents" in prompt and "missing config" in prompt and "2026-10-01" in prompt


def test_chat_report_is_parsed():
    text = ('Long analysis with every step.\n```json\n{"summary": "s", "root_cause": "r", "confidence": 0.9, '
            '"playbook": null, "chat_report": "  crashy crash-loops\\n**Root cause:** missing config  "}\n```')
    inv = parse_investigation(text, set())
    assert inv.chat_report == "crashy crash-loops\n**Root cause:** missing config"
    assert inv.analysis == "Long analysis with every step."
    assert parse_investigation('{"root_cause": "r", "chat_report": ""}', set()).chat_report is None
