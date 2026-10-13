---
name: container-oomkilled
description: Container killed for exceeding its memory limit (OOMKilled, exit code 137), memory leaks, KubeContainerOOMKilled alerts
---

## Goal
Decide between a too-low limit, a leak, and a load spike.

## Workflow
1. Confirm OOM: `lastState.terminated.reason == OOMKilled` on the container, or
   `kube_pod_container_status_last_terminated_reason{reason="OOMKilled"}`.
2. Read the limit and request: `kube_pod_container_resource_limits{resource="memory"}`.
3. Plot working set over the last 6-24 h:
   `container_memory_working_set_bytes{namespace="<ns>",pod=~"<workload>-.*",container="<c>"}`.
   - Saw-tooth growing to the limit after each restart: leak.
   - Flat well below the limit, then a sudden jump: load spike or a large request.
   - Always close to the limit: limit too low for normal operation.
4. Correlate with traffic and recent releases (image change, config change).
5. For JVM/Go/Node apps, compare heap settings with the container limit
   (`-Xmx`, `GOMEMLIMIT`, `--max-old-space-size`).
6. Check node pressure: OOM by the kernel at node level shows `MemoryPressure`
   on the node and evictions, not container OOMKilled.

## Synthesize Findings
Name the pattern (leak, spike, low limit) with the metric evidence and the limit value.

## Remediation
- Low limit: raise the limit in the workload spec (a change, not a playbook).
- Leak: restart buys time only; report it and escalate to the owners.
- Spike: scale out if the workload is horizontally scalable.

## Verify
No new OOMKilled terminations for 30 minutes and working set below 80% of the limit.
