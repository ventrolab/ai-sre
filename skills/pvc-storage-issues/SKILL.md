---
name: pvc-storage-issues
description: PersistentVolumeClaim Pending, volume mount or attach failures, volume full, KubePersistentVolumeFillingUp
---

## Workflow
1. PVC Pending: describe the PVC; check the StorageClass exists, its
   provisioner pods are healthy, and `volumeBindingMode` (WaitForFirstConsumer
   binds only when a pod is scheduled).
2. Mount/attach errors: pod events `FailedMount`/`FailedAttachVolume`; RWO
   volume still attached to another node after a pod moved.
3. Filling up: `kubelet_volume_stats_used_bytes / kubelet_volume_stats_capacity_bytes`
   and the growth rate; predict time to full with `predict_linear`.

## Remediation
Expand the PVC if the StorageClass allows expansion; clean data; fix the CSI
driver. Deleting data is never automatic.
