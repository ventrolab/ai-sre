---
name: image-pull-failure
description: ImagePullBackOff or ErrImagePull, wrong image tag, registry authentication or rate limits
---

## Workflow
1. Describe the pod; the `Failed` event carries the registry error.
2. Classify:
   - `not found` / `manifest unknown`: the tag does not exist; compare with the
     previous revision's image.
   - `unauthorized` / `authentication required`: imagePullSecrets missing, wrong
     namespace, or expired credentials.
   - `toomanyrequests`: registry rate limit (Docker Hub).
   - timeout / `no such host`: DNS or egress from the node.
3. Check if other pods on the same node pull from the same registry successfully.

## Remediation
Correct the tag or credentials, or roll back the image. No restart playbook
applies: the pull keeps failing until the cause is fixed.
