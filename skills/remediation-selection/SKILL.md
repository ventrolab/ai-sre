---
name: remediation-selection
description: How to choose a remediation playbook for an incident, when no playbook applies, and how risk and approvals work
---

## Rules
- A playbook is chosen only from the list offered in the question. Never invent
  commands or kubectl edits.
- Choose a playbook only when the evidence shows it addresses the root cause.
  Restarting a pod with a deterministic config or code error only repeats the failure.
- Prefer null with a clear explanation over a weak match.
- Risk classes: low may run automatically in autofix mode; medium may need
  approval; high always needs a human.
- If the root cause is unknown, the playbook is null and the confidence is low.

## Mapping
| Evidence | Playbook |
|---|---|
| Crash loop from a transient dependency or stuck state, config verified | restart-crashlooping-pod |
| Job failed permanently and blocks a CronJob | delete-failed-job |
| Memory leak confirmed, restart to buy time | restart-oom-deployment |
| CPU saturation, no HPA, scalable workload | scale-up-saturated-deployment |
| Node unhealthy, workloads should move | cordon-unhealthy-node |
| New release broken, previous revision healthy | rollback-failed-rollout |
| Host unit failed transiently, configuration valid | restart-failed-systemd-unit |
| Host filesystem filled by temp files or rotated logs | clean-host-disk |
