---
name: host-disk-full
description: Filesystem almost full on a host or node, NodeFilesystemSpaceFillingUp, NodeFilesystemAlmostOutOfSpace, inodes exhausted
---

## Workflow
1. Identify the filesystem and growth rate:
   `node_filesystem_avail_bytes{fstype!~"tmpfs|overlay"}` and
   `predict_linear(node_filesystem_avail_bytes[6h], 24*3600)`.
2. Check inodes too: `node_filesystem_files_free`.
3. Find what grows: logs (`/var/log`, journal size), container images and
   layers, application data, core dumps, temp directories.
4. On Kubernetes nodes, image garbage collection and evictions start under
   DiskPressure; check kubelet events.

## Remediation
- Allowlisted cleanup paths (temp, rotated logs): `clean-host-disk`.
- Application data or databases: never deleted automatically; escalate.
