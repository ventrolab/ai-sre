# k8s-agent

Read-only tools of one Kubernetes cluster for the Tool Hub: `k8s_list` (server-side
tables like kubectl get), `k8s_get` (YAML), `k8s_events`, `k8s_logs`, `prometheus_query`,
`prometheus_range_query`, `logs_backends`, `logs_query`, `logs_native`. Secrets are never
returned. It dials the hub (outbound WebSocket over TLS) and authenticates with an agent
token (default) or a client certificate (`auth: mtls`); its target name is the one its
credential was issued for.

The control cluster runs one as `localAgent` from `charts/ai-sre`. Remote clusters use
`charts/k8s-agent`:

```
# Join token: console, Administration -> Agents -> Add an agent (name = target name)
helm install ai-sre-agent charts/k8s-agent -n ai-sre --create-namespace \
  --set hubURL=wss://<tunnel ingress host>/v1/tunnel --set image.tag=<tag> \
  --set enrollment.token=<join token> --set labels.env=prod
```

An init container exchanges the join token for an agent token once and keeps it in the
Secret `<release>-token`. With the hub's own TLS port as `hubURL`
(`wss://<address>:8443/v1/tunnel`) add `--set enrollment.caSha256=<hub CA sha256>`.
Mutual TLS instead: `--set auth=mtls` with `enrollment.caSha256`, or certificates from
`scripts/issue-agent-cert.sh` in `tls.*`; the hub must allow it (`toolHub.tunnel.auth`).

Log backends (`logBackends`): `loki`, `victorialogs`, `elasticsearch` (with
`flavor: opensearch` for OpenSearch); `fields` maps selector names (namespace, pod,
container, host, unit, message, timestamp) to the backend's field or label names.
