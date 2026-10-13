"""Alertmanager alert -> Event normalization: target, correlation key, dedup id."""

from __future__ import annotations

import re
from typing import Any

from aisre_common.models import Event, Target

# Kubernetes generates name suffixes from an alphabet without vowels.
_H = "[bcdfghjklmnpqrstvwxz2456789]"
# <deployment>-<replicaset hash>-<pod hash>
_DEPLOYMENT_POD = re.compile(rf"^(?P<name>.+)-{_H}{{6,10}}-{_H}{{5}}$")
# <statefulset>-<ordinal>
_STATEFULSET_POD = re.compile(r"^(?P<name>.+)-\d+$")
# <daemonset or job>-<hash>
_SHORT_HASH_POD = re.compile(rf"^(?P<name>.+)-{_H}{{5}}$")

# <cronjob>-<scheduled time in minutes since the epoch>: the Job a CronJob creates.
_CRONJOB_JOB = re.compile(r"^(?P<name>.+)-\d{8,}$")

_WORKLOAD_LABELS = [
    ("deployment", "k8s.deployment"),
    ("statefulset", "k8s.statefulset"),
    ("daemonset", "k8s.daemonset"),
    ("job_name", "k8s.job"),
    ("cronjob", "k8s.cronjob"),
]


def cronjob_of(job: str) -> str | None:
    """The CronJob that created a Job, from the Job name; None for other Jobs."""
    match = _CRONJOB_JOB.match(job)
    return match.group("name") if match else None


def workload_from_pod(pod: str) -> str:
    for pattern in (_DEPLOYMENT_POD, _STATEFULSET_POD, _SHORT_HASH_POD):
        match = pattern.match(pod)
        if match:
            # Every run of a CronJob is a new Job: they all belong to the CronJob.
            return cronjob_of(match.group("name")) or match.group("name")
    return pod


def build_target(labels: dict[str, str], default_cluster: str) -> Target:
    target = Target(
        cluster=labels.get("cluster") or default_cluster,
        namespace=labels.get("namespace") or labels.get("exported_namespace"),
        pod=labels.get("pod") or labels.get("exported_pod"),
        node=labels.get("node"),
        labels={k: v for k, v in labels.items() if k in ("env", "team", "severity", "service")},
    )

    for label, kind in _WORKLOAD_LABELS:
        if labels.get(label):
            target.workload = labels[label]
            target.kind = kind
            if label == "job_name" and (cronjob := cronjob_of(target.workload)):
                target.workload, target.kind = cronjob, "k8s.cronjob"
            break

    if target.pod:
        if not target.workload:
            target.workload = workload_from_pod(target.pod)
        target.kind = "k8s.pod"
    elif target.kind == "unknown" and target.node:
        target.kind = "k8s.node"
    elif target.kind == "unknown" and not target.namespace:
        host = labels.get("host") or labels.get("hostname") or labels.get("instance")
        if host:
            target.host = host.split(":")[0]
            target.kind = "host.unit" if labels.get("name", "").endswith(".service") else "host"

    return target


def incident_key(alertname: str, target: Target) -> str:
    """Alerts about the same object share one incident."""
    if target.namespace and target.workload:
        scope = f"{target.namespace}/{target.workload}"
    elif target.namespace and target.pod:
        scope = f"{target.namespace}/{target.pod}"
    elif target.node:
        scope = f"node/{target.node}"
    elif target.host:
        scope = f"host/{target.host}"
    else:
        # Nothing to correlate on: one incident per alert name.
        scope = f"alert/{alertname}"
    return f"{target.cluster}/{scope}"


def alert_to_event(alert: dict[str, Any], default_cluster: str, external_url: str = "") -> tuple[Event, str]:
    """Returns the event and the JetStream deduplication id."""
    labels: dict[str, str] = alert.get("labels", {})
    alertname = labels.get("alertname", "unknown")
    target = build_target(labels, default_cluster)
    fingerprint = alert.get("fingerprint") or alertname
    status = alert.get("status", "firing")

    event = Event(
        kind="alert",
        source="alertmanager",
        target=target,
        fingerprint=fingerprint,
        incident_key=incident_key(alertname, target),
        payload={
            "alertname": alertname,
            "status": status,
            "labels": labels,
            "annotations": alert.get("annotations", {}),
            "starts_at": alert.get("startsAt"),
            "ends_at": alert.get("endsAt"),
            "generator_url": alert.get("generatorURL"),
            "alertmanager_url": external_url,
        },
    )
    dedup_id = f"{fingerprint}:{status}:{alert.get('startsAt', '')}"
    return event, dedup_id
