---
name: host-systemd-unit-failed
description: Systemd service failed or restarting on a standalone host (not a Kubernetes node), NodeSystemdServiceFailed
---

## Workflow
1. Read the unit state and the last 200 journal lines of the unit without a
   filter (`journalctl -u <unit>`), via the host tools.
2. Classify: configuration error after a change, missing dependency (disk,
   network, another unit), resource exhaustion (disk full, memory), or a crash.
3. Check disk and memory on the host (`host-disk-full` if a filesystem is full).
4. Check recent package updates or config changes if the host tools expose them.

## Remediation
- Transient failure with a healthy configuration: `restart-failed-systemd-unit`.
- Configuration errors: a restart fails again; report the exact error.
