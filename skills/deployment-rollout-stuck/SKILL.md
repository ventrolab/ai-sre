---
name: deployment-rollout-stuck
description: Deployment replicas mismatch, rollout stuck or progress deadline exceeded, KubeDeploymentReplicasMismatch, KubeDeploymentRolloutStuck
---

## Workflow
1. Read Deployment status: `replicas`, `updatedReplicas`, `availableReplicas`,
   conditions (`Progressing` reason `ProgressDeadlineExceeded`, `Available`).
2. Compare the current and previous ReplicaSets: which one has unready pods?
3. Investigate the unready pods of the new ReplicaSet with `pod-crashloop`,
   `image-pull-failure` or `pod-pending-scheduling`.
4. Check readiness probe failures in pod events.
5. Check PodDisruptionBudgets and HPA (an HPA may fight a manual replica count).

## Synthesize Findings
Say whether the new revision is broken (rollback candidate) or the cluster lacks
capacity (scheduling).

## Remediation
- New revision broken: `rollback-failed-rollout` (high risk, needs approval).
- Capacity: see `pod-pending-scheduling`.
