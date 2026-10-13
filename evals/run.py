#!/usr/bin/env python3
"""Evaluation runner: breaks real workloads on the cluster, sends the alert, scores the RCA.

    evals/run.py                        # all cases in evals/cases
    evals/run.py bad-rollout oom-low-limit --keep

For every case it creates a namespace `<case namespace>-<run id>`, applies the case steps
and waits until the workload is really broken, then posts a synthetic Alertmanager alert
to ingest with the label `aisre_eval=<run id>`. The controller investigates it like any
other alert but sends nothing to chat, never remediates and never uses evaluation
incidents as similar-incident hints. The runner waits for the diagnosis, scores it
against the case expectations, resolves the alert and removes the namespace and the
incident (unless --keep). Results go to evals/results/<time>-<provider>.json and .md.

Needs kubectl access to the cluster that runs the release (KUBECONFIG) and PyYAML.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import secrets
import socket
import subprocess
import sys
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import yaml

ROOT = Path(__file__).resolve().parent
EVAL_LABEL = "aisre_eval"  # = aisre_common.similar.EVAL_LABEL
DONE_STATUSES = {"diagnosed", "awaiting_approval", "remediating", "remediated", "remediation_failed",
                 "investigation_failed", "resolved", "closed"}


def log(msg: str) -> None:
    print(f"{datetime.now().strftime('%H:%M:%S')} {msg}", file=sys.stderr, flush=True)


class Cluster:
    def __init__(self, namespace: str, release: str):
        self.ns, self.release = namespace, release
        self.pf: subprocess.Popen | None = None
        self.ingest_port = 0

    def kubectl(self, *args: str, stdin: str | None = None, check: bool = True) -> str:
        res = subprocess.run(["kubectl", *args], input=stdin, capture_output=True, text=True)
        if check and res.returncode != 0:
            raise RuntimeError(f"kubectl {' '.join(args[:3])}: {res.stderr.strip()}")
        return res.stdout

    def sql(self, query: str) -> list[dict[str, Any]]:
        """Runs a query in the release database; returns rows as dicts."""
        wrapped = f"select coalesce(json_agg(t), '[]') from ({query}) t"
        out = self.kubectl("-n", self.ns, "exec", f"{self.release}-postgres-0", "--", "psql", "-U", "aisre",
                           "-d", "aisre", "-tA", "-c", wrapped)
        return json.loads(out.strip() or "[]")

    def exec_sql(self, statement: str) -> None:
        self.kubectl("-n", self.ns, "exec", f"{self.release}-postgres-0", "--", "psql", "-U", "aisre",
                     "-d", "aisre", "-qtA", "-c", statement)

    def secret(self, name: str, key: str) -> str:
        data = self.kubectl("-n", self.ns, "get", "secret", name, "-o", f"jsonpath={{.data.{key}}}")
        return base64.b64decode(data).decode()

    def helm_values(self) -> dict[str, Any]:
        res = subprocess.run(["helm", "-n", self.ns, "get", "values", self.release, "-a", "-o", "json"],
                             capture_output=True, text=True)
        return json.loads(res.stdout) if res.returncode == 0 else {}

    def start_ingest_forward(self) -> None:
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            self.ingest_port = s.getsockname()[1]
        self.pf = subprocess.Popen(
            ["kubectl", "-n", self.ns, "port-forward", f"svc/{self.release}-ingest", f"{self.ingest_port}:8080"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for _ in range(50):
            try:
                socket.create_connection(("127.0.0.1", self.ingest_port), timeout=0.2).close()
                return
            except OSError:
                time.sleep(0.2)
        raise RuntimeError("port-forward to ingest failed")

    def stop(self) -> None:
        if self.pf:
            self.pf.terminate()


@dataclass
class Result:
    case: str
    namespace: str
    attempt: int = 1
    passed: bool = False
    error: str | None = None
    incident_id: str | None = None
    status: str | None = None
    root_cause: str | None = None
    confidence: float | None = None
    playbook: str | None = None
    expected_playbook: str | None = None
    root_cause_score: float = 0.0
    missing_keywords: list[list[str]] = field(default_factory=list)
    checks: dict[str, bool] = field(default_factory=dict)
    seconds: float | None = None
    tool_calls: int | None = None
    tools: list[str] = field(default_factory=list)
    playbooks_offered: list[str] = field(default_factory=list)
    playbook_reason: str | None = None


def pod_reasons(pod: dict[str, Any]) -> set[str]:
    """Every state reason Kubernetes reports for a pod and its containers."""
    st = pod.get("status", {})
    out = {st.get("phase", ""), st.get("reason", "")}
    out |= {c.get("reason", "") for c in st.get("conditions", [])}
    for c in st.get("containerStatuses", []) + st.get("initContainerStatuses", []):
        for state in (c.get("state", {}), c.get("lastState", {})):
            for v in state.values():
                out.add(v.get("reason", ""))
        if c.get("ready"):
            out.add("ContainerReady")
    return out - {""}


def wait_for(cl: Cluster, ns: str, cond: dict[str, Any], timeout: int) -> dict[str, str]:
    """Waits for a step condition; returns the labels that identify the broken object."""
    deadline, held_since = time.monotonic() + timeout, None
    while time.monotonic() < deadline:
        found: dict[str, str] | None = None
        if "job" in cond:
            job = json.loads(cl.kubectl("-n", ns, "get", "job", cond["job"], "-o", "json", check=False) or "{}")
            if any(c.get("type") == cond["condition"] and c.get("status") == "True"
                   for c in job.get("status", {}).get("conditions", [])):
                found = {"job_name": cond["job"]}
        else:
            pods = json.loads(cl.kubectl("-n", ns, "get", "pods", "-l", cond["selector"], "-o", "json"))["items"]
            for pod in pods:
                reasons = pod_reasons(pod)
                statuses = pod.get("status", {}).get("containerStatuses", [])
                ready = bool(statuses) and all(c.get("ready") for c in statuses)
                if (cond.get("ready") and ready) or set(cond.get("reasons", [])) & reasons:
                    found = {"pod": pod["metadata"]["name"]}
                    break
        if found:
            held_since = held_since or time.monotonic()
            if time.monotonic() - held_since >= cond.get("hold", 0):
                return found
        else:
            held_since = None
        time.sleep(3)
    raise TimeoutError(f"condition {cond} not reached in {timeout}s")


def post_alert(cl: Cluster, token: str, alert: dict[str, Any]) -> None:
    body = json.dumps({"version": "4", "status": alert["status"], "alerts": [alert]}).encode()
    req = urllib.request.Request(f"http://127.0.0.1:{cl.ingest_port}/v1/alerts/alertmanager", data=body,
                                 headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        resp.read()


def score(case: dict[str, Any], r: Result) -> None:
    exp = case.get("expect", {})
    text = (r.root_cause or "").lower()
    groups = exp.get("root_cause", [])
    r.missing_keywords = [g for g in groups if not any(str(k).lower() in text for k in g)]
    r.root_cause_score = round(1 - len(r.missing_keywords) / len(groups), 2) if groups else 1.0
    r.checks["diagnosed"] = r.status not in (None, "investigation_failed") and r.root_cause is not None
    r.checks["root_cause"] = not r.missing_keywords
    r.checks["confidence"] = (r.confidence or 0) >= exp.get("min_confidence", 0)
    if "playbook" in exp:
        r.expected_playbook = exp["playbook"]
        r.checks["playbook"] = r.playbook == exp["playbook"]
    r.passed = all(r.checks.values())


def run_case(cl: Cluster, case: dict[str, Any], attempt: int, run_id: str, token: str, started: str,
             timeout: int, keep: bool) -> Result:
    ns = f"{case['namespace']}-{run_id}-{attempt}"
    r = Result(case=case["name"], namespace=ns, attempt=attempt)
    alert: dict[str, Any] | None = None
    try:
        cl.kubectl("create", "namespace", ns)
        cl.kubectl("label", "namespace", ns, f"{EVAL_LABEL}={run_id}")
        ident: dict[str, str] = {}
        for i, step in enumerate(case["steps"], 1):
            cl.kubectl("-n", ns, "apply", "-f", "-", stdin=step["apply"])
            if step.get("wait"):
                ident = wait_for(cl, ns, step["wait"], step["wait"].get("timeout", 300))
            log(f"{case['name']}: step {i}/{len(case['steps'])} ready {ident}")

        a = case["alert"]
        labels = {"alertname": a["alertname"], "namespace": ns, EVAL_LABEL: run_id, **ident, **a.get("labels", {})}
        fmt = {**labels, "job": ident.get("job_name", "")}
        alert = {
            "status": "firing",
            "labels": labels,
            "annotations": {"summary": a.get("summary", "").format(**fmt)},
            "startsAt": datetime.now(timezone.utc).isoformat(),
            "fingerprint": f"eval-{run_id}-{case['name']}-{attempt}",
        }
        post_alert(cl, token, alert)
        log(f"{case['name']}: alert {a['alertname']} sent")

        deadline = time.monotonic() + timeout
        row: dict[str, Any] = {}
        while time.monotonic() < deadline:
            rows = cl.sql(f"""select id, status, root_cause, confidence, playbook from incidents
                              where target->>'namespace' = '{ns}' and created_at >= '{started}'
                              order by created_at limit 1""")
            row = rows[0] if rows else {}
            if row.get("status") in DONE_STATUSES:
                break
            time.sleep(5)
        else:
            raise TimeoutError(f"no diagnosis in {timeout}s (status {row.get('status')})")

        r.incident_id, r.status = row["id"], row["status"]
        r.root_cause, r.confidence, r.playbook = row["root_cause"], row["confidence"], row["playbook"]
        stats = cl.sql(f"""select data from incident_events where incident_id = '{r.incident_id}'
                           and type = 'investigation_stats' order by ts desc limit 1""")
        if stats:
            d = stats[0]["data"]
            r.seconds, r.tool_calls, r.tools = d.get("seconds"), d.get("tool_calls"), d.get("tools", [])
        choice = cl.sql(f"""select data from incident_events where incident_id = '{r.incident_id}'
                            and type = 'playbook_choice' order by ts desc limit 1""")
        if choice:
            r.playbooks_offered = choice[0]["data"].get("offered", [])
            r.playbook_reason = choice[0]["data"].get("reason")
        score(case, r)
        log(f"{case['name']}: {'PASS' if r.passed else 'FAIL'} {r.checks}")
    except Exception as exc:
        r.error = str(exc)
        log(f"{case['name']}: ERROR {exc}")
    finally:
        if alert:
            try:
                post_alert(cl, token, {**alert, "status": "resolved", "endsAt": datetime.now(timezone.utc).isoformat()})
            except Exception as exc:
                log(f"{case['name']}: resolve failed: {exc}")
        if not keep:
            cl.kubectl("delete", "namespace", ns, "--wait=false", check=False)
            if r.incident_id:
                cl.exec_sql(f"delete from incidents where id = '{r.incident_id}'")
    return r


def report_md(meta: dict[str, Any], results: list[Result]) -> str:
    passed = sum(r.passed for r in results)
    lines = [
        f"# Evaluation {meta['run_id']}",
        "",
        f"- Time: {meta['started']}",
        f"- Provider: {meta['provider']} ({meta['model']}), fallbacks: {', '.join(meta['fallbacks']) or 'none'}",
        f"- Passed: {passed}/{len(results)} ({meta['repeat']} run(s) per case)",
        "",
        "| Case | Passed | Root cause | Playbook | Confidence | Seconds | Tool calls | Errors |",
        "|---|---|---|---|---|---|---|---|",
    ]
    by_case: dict[str, list[Result]] = {}
    for r in results:
        by_case.setdefault(r.case, []).append(r)
    for name, rs in by_case.items():
        def avg(values: list[float | None], fmt: str) -> str:
            vals = [v for v in values if v is not None]
            return format(sum(vals) / len(vals), fmt) if vals else "-"
        playbooks = ", ".join(sorted({r.playbook or "none" for r in rs if not r.error})) or "-"
        if any(r.checks.get("playbook") is False for r in rs):
            playbooks += f" (want {rs[0].expected_playbook or 'none'})"
        lines.append(
            f"| {name} | {sum(r.passed for r in rs)}/{len(rs)} | {avg([r.root_cause_score for r in rs if not r.error], '.2f')} "
            f"| {playbooks} | {avg([r.confidence for r in rs], '.2f')} | {avg([r.seconds for r in rs], '.1f')} "
            f"| {avg([r.tool_calls for r in rs], '.1f')} | {sum(bool(r.error) for r in rs)} |")
    for r in results:
        if r.error or not r.passed:
            lines += ["", f"## {r.case} (run {r.attempt})", "",
                      f"- Error: {r.error}" if r.error else f"- Checks: {r.checks}"]
            if r.missing_keywords:
                lines.append(f"- Missing keyword groups: {r.missing_keywords}")
            if r.root_cause:
                lines.append(f"- Root cause: {r.root_cause}")
            if r.playbook_reason:
                lines.append(f"- Playbooks offered: {', '.join(r.playbooks_offered) or 'none'}; "
                             f"reason given: {r.playbook_reason}")
    return "\n".join(lines) + "\n"


def rescore(path: Path, cases: dict[str, dict[str, Any]]) -> int:
    data = json.loads(path.read_text())
    known = set(Result.__dataclass_fields__)
    results = [Result(**{k: v for k, v in r.items() if k in known}) for r in data.pop("results")]
    for r in results:
        if not r.error and r.case in cases:
            r.checks, r.missing_keywords = {}, []
            score(cases[r.case], r)
    data.setdefault("repeat", max(r.attempt for r in results))
    data.setdefault("fallbacks", [])
    md = report_md(data, results)
    path.write_text(json.dumps({**data, "results": [asdict(r) for r in results]}, indent=2) + "\n")
    path.with_suffix(".md").write_text(md)
    print(md)
    return 0 if all(r.passed for r in results) else 1


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("cases", nargs="*", help="case names (default: all)")
    ap.add_argument("--namespace", default=os.environ.get("NS", "ai-sre"), help="release namespace")
    ap.add_argument("--release", default=os.environ.get("RELEASE", "ai-sre"))
    ap.add_argument("--timeout", type=int, default=900, help="seconds to wait for a diagnosis per case")
    ap.add_argument("--parallel", type=int, default=4, help="cases set up at the same time")
    ap.add_argument("--repeat", type=int, default=1, help="runs per case (answers vary between runs)")
    ap.add_argument("--keep", action="store_true", help="keep namespaces and incidents for inspection")
    ap.add_argument("--label", default="", help="extra label for the result file name")
    ap.add_argument("--out", default=str(ROOT / "results"))
    ap.add_argument("--rescore", metavar="RESULT_JSON",
                    help="re-score a saved result with the current case expectations (no cluster needed)")
    args = ap.parse_args()

    cases = [yaml.safe_load(p.read_text()) for p in sorted((ROOT / "cases").glob("*.yaml"))]
    if args.rescore:
        return rescore(Path(args.rescore), {c["name"]: c for c in cases})
    if args.cases:
        unknown = set(args.cases) - {c["name"] for c in cases}
        if unknown:
            ap.error(f"unknown cases: {', '.join(sorted(unknown))}")
        cases = [c for c in cases if c["name"] in args.cases]

    cl = Cluster(args.namespace, args.release)
    llm = cl.helm_values().get("llm", {})
    provider = llm.get("provider", "unknown")
    meta = {
        "run_id": secrets.token_hex(3),
        "started": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "provider": provider,
        "model": (llm.get("models", {}).get(provider) or {}).get("model", "unknown"),
        "fallbacks": llm.get("fallbacks") or [],
        "repeat": args.repeat,
    }
    log(f"run {meta['run_id']}: {len(cases)} cases, provider {provider} ({meta['model']})")
    token = cl.secret(f"{args.release}-secrets", "INGEST_TOKEN")
    cl.start_ingest_forward()
    try:
        with ThreadPoolExecutor(max_workers=args.parallel) as pool:
            jobs = [(c, k) for k in range(1, args.repeat + 1) for c in cases]
            results = list(pool.map(
                lambda j: run_case(cl, j[0], j[1], meta["run_id"], token, meta["started"], args.timeout, args.keep),
                jobs))
        results.sort(key=lambda r: (r.case, r.attempt))
    finally:
        cl.stop()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    stem = f"{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}-{provider}" + (f"-{args.label}" if args.label else "")
    md = report_md(meta, results)
    (out / f"{stem}.json").write_text(json.dumps({**meta, "results": [asdict(r) for r in results]}, indent=2) + "\n")
    (out / f"{stem}.md").write_text(md)
    print(md)
    log(f"results: {out / stem}.json")
    return 0 if all(r.passed for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
