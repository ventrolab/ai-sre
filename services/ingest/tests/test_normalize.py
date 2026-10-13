from app.normalize import alert_to_event, build_target, workload_from_pod


def test_workload_from_pod():
    assert workload_from_pod("api-7d4b9c8f6d-x2k9p") == "api"
    assert workload_from_pod("postgres-0") == "postgres"
    assert workload_from_pod("node-exporter-x7k2p") == "node-exporter"
    assert workload_from_pod("standalone") == "standalone"


def test_pod_alert_target_and_key():
    alert = {
        "status": "firing",
        "fingerprint": "f1",
        "startsAt": "2026-10-11T10:00:00Z",
        "labels": {
            "alertname": "KubePodCrashLooping",
            "namespace": "demo",
            "pod": "crashy-7d4b9c8f6d-x2k9p",
            "container": "app",
        },
    }
    event, dedup = alert_to_event(alert, "testbed")
    assert event.target.kind == "k8s.pod"
    assert event.target.workload == "crashy"
    assert event.target.cluster == "testbed"
    assert event.incident_key == "testbed/demo/crashy"
    assert dedup == "f1:firing:2026-10-11T10:00:00Z"


def test_related_alerts_share_incident():
    a, _ = alert_to_event(
        {"labels": {"alertname": "KubePodCrashLooping", "namespace": "demo", "pod": "crashy-7d4b9c8f6d-x2k9p"}},
        "c",
    )
    b, _ = alert_to_event(
        {"labels": {"alertname": "KubeDeploymentReplicasMismatch", "namespace": "demo", "deployment": "crashy"}},
        "c",
    )
    assert a.incident_key == b.incident_key
    assert b.target.kind == "k8s.deployment"


def test_host_unit_target():
    target = build_target(
        {"alertname": "NodeSystemdServiceFailed", "instance": "db1.example.com:9100", "name": "nginx.service"},
        "c",
    )
    assert target.kind == "host.unit"
    assert target.host == "db1.example.com"


def test_resolved_has_distinct_dedup_id():
    base = {"fingerprint": "f", "startsAt": "t", "labels": {"alertname": "X"}}
    _, firing = alert_to_event({**base, "status": "firing"}, "c")
    _, resolved = alert_to_event({**base, "status": "resolved"}, "c")
    assert firing != resolved


def test_cronjob_runs_belong_to_the_cronjob():
    from app.normalize import build_target, workload_from_pod

    t = build_target({"alertname": "KubeJobFailed", "namespace": "shop", "job_name": "nightly-report-29864160"}, "c")
    assert (t.workload, t.kind) == ("nightly-report", "k8s.cronjob")
    assert workload_from_pod("nightly-report-29864160-qnfx2") == "nightly-report"
    # A plain Job keeps its name; digits that are not a schedule stay too.
    t = build_target({"namespace": "shop", "job_name": "migrate-v2"}, "c")
    assert (t.workload, t.kind) == ("migrate-v2", "k8s.job")
    assert workload_from_pod("web-7d9c5b6f4d-x2kqp") == "web"
