---
name: service-errors-latency
description: HTTP 5xx error rate, high latency, SLO burn, service has no endpoints, ingress returning 502/503/504
---

## Workflow
1. Quantify: error ratio and latency percentiles before and during the incident
   (ingress or service metrics, e.g. `nginx_ingress_controller_requests`,
   `http_requests_total`, histogram `_bucket` series).
2. Scope: one route, one backend, one pod, or everything behind the ingress.
3. Endpoints: does the Service have ready endpoints? 503 with no endpoints means
   all pods are unready.
4. Pods: restarts, CPU throttling (`container_cpu_cfs_throttled_periods_total`),
   memory, recent rollout.
5. Dependencies: errors or latency of databases, caches and downstream APIs in
   the application logs.
6. Read application logs around the first errors, without a filter first.

## Synthesize Findings
Point to the first component where errors start; give the timeline.

## Remediation
Roll back a bad release (approval), scale out a saturated service
(`scale-up-saturated-deployment`), or escalate a failing dependency.
