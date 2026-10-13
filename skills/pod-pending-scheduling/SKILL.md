---
name: pod-pending-scheduling
description: Pods stuck in Pending, FailedScheduling events, insufficient CPU or memory, taints, affinity, unbound PVC
---

## Workflow
1. Describe the pending pod and read the `FailedScheduling` event message; it
   lists why each node was rejected.
2. Map the reason:
   - `Insufficient cpu/memory`: compare requests with node allocatable minus
     requested (`kube_node_status_allocatable`, `kube_pod_container_resource_requests`).
   - `node(s) had untolerated taint`: compare pod tolerations with node taints.
   - `didn't match Pod's node affinity/selector`: check nodeSelector/affinity labels.
   - `pod has unbound immediate PersistentVolumeClaims`: follow `pvc-storage-issues`.
   - `Too many pods`: node pod limit.
3. Check whether nodes are NotReady or cordoned (`kube_node_spec_unschedulable`).
4. Check for a ResourceQuota or LimitRange in the namespace blocking creation
   (that shows as a ReplicaSet event, not a Pending pod).

## Synthesize Findings
Quote the scheduler message and the numbers (requested vs free).

## Remediation
Free capacity, fix selectors/tolerations, or add nodes. Cordon/uncordon and
scaling decisions need approval.
