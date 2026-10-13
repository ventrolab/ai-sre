---
name: node-not-ready
description: Kubernetes node NotReady or unreachable, MemoryPressure, DiskPressure, PIDPressure, kubelet problems, KubeNodeNotReady
---

## Workflow
1. Read node conditions and their `lastTransitionTime` and messages.
2. Pressure conditions:
   - DiskPressure: `node_filesystem_avail_bytes` on `/` and the container
     runtime volume; image and log usage; evicted pods.
   - MemoryPressure: `node_memory_MemAvailable_bytes`, top pods by working set.
   - PIDPressure: `node_processes_threads` or pids per pod.
3. NotReady/Unknown: kubelet heartbeat missing. Check whether node-exporter and
   kubelet metrics still arrive (`up{job=~".*node.*|kubelet"}`), network
   reachability, and recent reboots (`node_boot_time_seconds`).
4. List pods that were evicted or are terminating on the node.

## Remediation
- Disk: clean images/logs (host playbooks), expand the disk.
- Unhealthy node with workloads: `cordon-unhealthy-node` (approval), then drain
  only with approval.
