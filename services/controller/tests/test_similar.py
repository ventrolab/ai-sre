from aisre_common import similar


def test_tsquery_uses_title_alerts_and_target():
    q = similar.tsquery("KubePodCrashLooping on demo/crashy-app",
                        [{"alertname": "KubePodCrashLooping"}, {"alertname": "KubeDeploymentReplicasMismatch"}],
                        {"kind": "k8s.pod", "namespace": "demo", "workload": "crashy-app"})
    words = q.split(" | ")
    assert "'KubePodCrashLooping'" in words and "'KubeDeploymentReplicasMismatch'" in words
    assert "'crashy'" in words and "'app'" in words
    assert "'demo'" not in words and "'k8s.pod'" not in words
    # Short words are dropped, duplicates removed, quotes and tsquery operators stripped.
    assert "'on'" not in words and len(words) == len(set(words))
    assert similar.tsquery("it's (a) b&c!", [], None) == "'its'"


def test_tsquery_empty():
    assert similar.tsquery("", [], {}) == ""
