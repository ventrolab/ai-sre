---
name: pod-crashloop
description: Pod or container in CrashLoopBackOff, repeated restarts, KubePodCrashLooping alerts
---

## Goal
Find why the container keeps exiting and whether a restart can help.

## Workflow
1. Resolve the current pods of the workload. The pod in the alert may be gone:
   list pods of the owning Deployment/StatefulSet/DaemonSet in the namespace.
2. Describe one current pod. Record per container: `lastState.terminated.reason`,
   `exitCode`, `restartCount`, and pod events (probe failures, mount errors).
3. Read logs WITHOUT a filter for the current and the previous container
   instance. Filters are case-sensitive and hide the real message.
4. Classify by the termination reason and exit code:
   - `OOMKilled` or exit 137 with memory near the limit: follow `container-oomkilled`.
   - `Error` with exit 1/2 and a message in the logs: application or configuration error.
   - Exit 137 without OOM: liveness probe kill or external SIGKILL; check probe events.
   - Exit 0 in a loop: the command finishes; the process is not a long-running server.
   - `ContainerCannotRun`, `CreateContainerConfigError`: missing Secret/ConfigMap key,
     bad command, image architecture.
5. Check what changed: Deployment revision history, image tag, ConfigMap/Secret
   references and whether they exist.
6. If the app depends on another service (DB, queue, API), check that service's
   endpoints and recent errors.

## Synthesize Findings
- Quote the exact fatal log line and the termination reason.
- State whether all replicas fail identically (deterministic: config/code) or
  only some (node, dependency, resource contention).
- If the earlier pods named in the alert are gone, say so; do not treat
  NotFound as the cause.

## Remediation
- Deterministic config/code errors: a restart does not help. Recommend fixing
  the config or rolling back; do not choose a restart playbook.
- Transient causes (dependency was down and is back, stuck state cleared by a
  restart): `restart-crashlooping-pod` is appropriate.
- Bad new release: a rollback playbook, which needs approval.

## Verify
`increase(kube_pod_container_status_restarts_total{namespace="<ns>",pod=~"<workload>-.*"}[10m]) == 0`
and the Deployment has all replicas available.
