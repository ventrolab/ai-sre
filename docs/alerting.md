# Alerting: which alerts go to AI SRE

AI SRE receives alerts from Alertmanager through a webhook receiver. It does not have
to receive every alert, and usually should not: an investigation costs LLM tokens and
time, and many alerts already say everything there is to know.

## Which alerts benefit from an investigation

| Send to AI SRE | Keep out |
|---|---|
| The alert says *that* something is wrong, the cause needs digging: high latency or TTFB, 5xx rate, a full or fast-filling disk (what fills it), high CPU (what loads it), out-of-sync or degraded replicated storage, pods stuck in Pending, failed jobs, crash loops, backends without healthy endpoints | The alert is the whole story: host down, exporter not reachable, hardware failure (RAID disk failed), certificate expiry, status of an external service |
| A fix may be a known playbook (restart, rollback, scale, clean up) | Nobody can act on it from inside the cluster or host |

Rule of thumb: if the first thing an engineer does after the alert is open dashboards,
logs and events to find out *why*, the alert should go to AI SRE.

## Opt-in label

Mark the rules that need an investigation with a label, and route only those to AI SRE.
The decision then lives next to the alert rule, and new rules opt in without touching
the Alertmanager configuration.

Alert rule (Prometheus `PrometheusRule`, VictoriaMetrics `VMRule` or a plain rules file):

```yaml
- alert: HighTTFB
  expr: histogram_quantile(0.95, sum by (le, service) (rate(http_ttfb_seconds_bucket[5m]))) > 1
  for: 10m
  labels:
    severity: warning
    ai_sre: investigate
  annotations:
    summary: "TTFB p95 of {{ $labels.service }} is above 1s"
```

Alertmanager: AI SRE as the first child route with `continue: true`, so the alert still
reaches the usual receivers:

```yaml
receivers:
  - name: ai-sre
    webhook_configs:
      - url: http://<release>-ingest.<namespace>.svc:8080/v1/alerts/alertmanager
        send_resolved: true
        http_config:
          authorization:
            credentials_file: /etc/alertmanager/secrets/ai-sre/token   # INGEST_TOKEN
  - name: chat
    # ... your existing receiver

route:
  receiver: chat
  routes:
    - receiver: ai-sre
      continue: true
      matchers:
        - ai_sre="investigate"
      group_by: ['...']       # one notification per alert; AI SRE groups them itself
      group_wait: 30s
      group_interval: 5m
      repeat_interval: 4h
    # ... your existing routes
```

`send_resolved: true` is required: AI SRE closes an incident when all its alerts are
resolved.

Alternatives when rules cannot carry the label (for example rule files copied unchanged
from upstream): a separate route with `matchers: [alertname=~"DiskRunsOutOfSpace|..."]`
and `continue: true`, or a label added by the alerting engine (`external_labels`,
relabeling of alerts).

## Keep a direct path

AI SRE is an extra step, not a replacement for the alerting path:

- Every alert should still reach a human receiver directly. If AI SRE, its cluster or
  the messenger bot is down, alerts must not disappear.
- Alerts about AI SRE itself (pods not ready, no agents connected, investigations
  failing) and about the cluster it runs in must never depend on it.
- A dead man's switch (an always-firing Watchdog alert sent to an external heartbeat
  service) shows that the whole alerting path works.

## What AI SRE sends to chat

- `Incident: ...` as soon as an alert opens an incident, before the investigation.
- The RCA when the investigation is done (root cause, confidence, proposed playbook).
- `Resolved: ...` when all alerts of the incident are resolved.

Alerts about the same object (workload, pod, node or host) are correlated into one open
incident. Another alert for an object that already has an open incident is added to it
without a new chat message, and Alertmanager reminders (`repeat_interval`) do not create
new messages either. That is one more reason to keep the direct receiver.

## Pitfalls

- **Adding or removing the label changes the alert's identity.** Alertmanager sees a new
  alert, and the old one resolves - but the resolve of the old alert now goes only to the
  routes it still matches. Incidents opened by the old alert in AI SRE would then wait
  for a resolve that never comes. When you narrow the route, close the open incidents
  of alerts that no longer match. The console has no close action yet; in the database:
  mark the alerts without the label resolved and set the incident status to `closed`.
- **Host-level causes need a host agent.** For a full disk or high CPU on a machine,
  the alert usually names it by `instance`/`host`. Without a host-agent of that name AI
  SRE sees metrics only (filesystem usage per mount point, CPU modes, per-pod usage on
  Kubernetes nodes), not which directory fills the disk or which process loads the CPU.
  Give the host agent the commands for that (`du -xh --max-depth=2 /`,
  `ps aux --sort=-%cpu`).
- **Grouping.** `group_by: ['...']` sends each alert separately; AI SRE correlates them
  into incidents. Grouping in Alertmanager would hide individual targets.
