# Ventrolab AI SRE - Architecture

Status: implemented and running; delivery phases and open work in section 11.

Diagram: [docs/architecture.html](docs/architecture.html) (source: `docs/architecture.json`, built with archify)

## 1. Purpose

Ventrolab AI SRE is a standalone, Kubernetes-native product. It is not tied to any specific company or project: it can be installed into any environment, and everything environment-specific (targets, playbooks, policies, messengers, LLM provider) is configuration.

It:

- receives alerts and scheduled health-check tasks;
- investigates incidents using metrics, logs, events and host state;
- produces a root cause analysis (RCA) and a remediation proposal;
- optionally executes remediation (autofix), with guardrails and human approval for risky actions;
- reaches not only its own cluster, but also remote Kubernetes clusters and standalone hosts;
- uses a pluggable LLM core: Claude, OpenAI or a self-hosted model (Ollama / vLLM).

## 2. Design principles

1. **Reuse, do not rebuild the investigator.** HolmesGPT is the investigation engine. Our code owns the incident lifecycle, remediation and integrations around it.
2. **Read and write are separate paths.** Investigation uses read tools only. Every write action goes through a single enforcement point - the Action Guard.
3. **Mode is configuration, not code.** `SRE_MODE=readonly|autofix` switches behavior without redeploying different code.
4. **Provider-agnostic LLM.** All model calls go through an OpenAI-compatible gateway. Changing the provider is a config change.
5. **Pluggable edges.** Messengers, tool servers (MCP) and log/metric backends are plugins behind stable internal interfaces.
6. **Outbound-only remote access.** Remote clusters and hosts run an agent that dials the control cluster. No inbound ports, no shared kubeconfigs.
7. **Everything is audited.** Every LLM call, tool call and action is traceable to an incident.
8. **Behavior is data.** Remediation playbooks, risk classes and policies are an editable catalog loaded at runtime, not compiled into the services.
9. **Product, not a project.** No assumptions about a specific organization, naming or topology. One installation serves one operator team and the targets it registers.

## 3. Components

| Component | Responsibility | Implementation |
|---|---|---|
| Event Ingest | Receives Alertmanager webhooks, scheduler tasks and chat commands; deduplicates, correlates, enriches, publishes events | Own service |
| Event Queue | Durable event bus between ingest, controller and chat gateway | NATS JetStream |
| Scheduler | Periodic proactive health checks | Kubernetes CronJob(s) |
| SRE Controller | Incident state machine, investigation orchestration, remediation playbooks, notifications | Own service (core) |
| HolmesGPT | Investigation and RCA, read-only tool use | Upstream HolmesGPT (server mode) |
| LLM Gateway | Single OpenAI-compatible endpoint, provider routing, fallback, budgets | LiteLLM proxy |
| LLM providers | Model inference | Claude API, OpenAI API, Ollama / vLLM |
| Action Guard | Enforces `SRE_MODE`, risk classes, approvals, rate limits, circuit breaker | Own service |
| Tool Hub | Registry and proxy for MCP servers (k8s, Prometheus, log backends, host); routes calls to local or remote targets | Own service + MCP servers |
| Chat Gateway | Messenger-agnostic notifications and commands; adapter plugins | Own service |
| k8s-agent | Runs in each remote cluster; executes tool calls locally; outbound tunnel (agent token or mTLS) | Own binary |
| host-agent | Runs on standalone hosts; allowlisted commands and file reads; outbound tunnel (agent token or mTLS) | Own binary |
| PostgreSQL + pgvector | Incidents, actions, approvals; past incidents for retrieval | PostgreSQL |
| Vault | Short-lived credentials for tools and agents | HashiCorp Vault (or External Secrets) |
| Audit & Traces | LLM traces, tool-call audit, action audit | Langfuse + OpenTelemetry |

### 3.1 Event Ingest

- Inputs:
  - Alertmanager webhook (`POST /v1/alerts/alertmanager`), from any cluster;
  - scheduler tasks (`POST /v1/checks`);
  - chat commands and approvals from the Chat Gateway.
- Normalizes all inputs into one `Event` envelope (see 5.1).
- Deduplication by alert fingerprint + time window.
- Correlation: groups related alerts (same cluster/namespace/workload/host within a window) into one incident key.
- Enrichment: adds target metadata (cluster, environment, owner, runbook links).

### 3.2 SRE Controller

The controller is the only component that owns incident state.

Incident lifecycle:

```
new -> investigating -> diagnosed -> (remediation_proposed)
    -> (awaiting_approval) -> remediating -> verifying -> resolved
                                                       \-> remediation_failed -> rolled_back -> escalated
any state -> escalated | closed
```

Responsibilities:

- consumes events from the queue, creates or updates incidents;
- calls HolmesGPT with the alert context and the applicable playbooks; Holmes pulls troubleshooting guides from the knowledge base (section 3.10) itself; similar past incidents from pgvector come later;
- maps the RCA to a remediation playbook and builds an action plan;
- sends the action plan to the Action Guard;
- notifies the Chat Gateway about RCA, proposals, approvals and results;
- stores the full timeline in PostgreSQL.

### 3.3 HolmesGPT integration

- Deployed as a server inside the control cluster.
- The controller calls its HTTP investigation API (`/api/chat`) with: alert payload, target identity, current time, applicable playbooks.
- Requests carry `Accept: application/json`: Holmes passes request headers to its tools and refuses to run kubectl when a header contains shell metacharacters such as the default `*/*`.
- Holmes uses the LLM Gateway as its model endpoint (OpenAI-compatible base URL).
- Holmes toolsets:
  - built-in toolsets for the local cluster where useful;
  - Tool Hub MCP servers for everything else (remote clusters, hosts, extra backends).
- Holmes is given **read tools only**. It never receives write tools.
- The expected output is an RCA plus a structured remediation proposal (action type, target, parameters, confidence). The controller validates this output; it never executes free-form text.

### 3.4 LLM Gateway

- LiteLLM proxy exposes one OpenAI-compatible endpoint.
- Provider and model are selected by configuration:
  - `LLM_PROVIDER=claude|openai|ollama`
  - `LLM_MODEL=<model id>`
  - `LLM_FALLBACK=<ordered list>`
- Optional routing by data sensitivity: incidents tagged as sensitive are forced to the self-hosted model, so no data leaves the infrastructure.
- Budgets and rate limits per consumer (Holmes, controller summaries).
- Note: tool calling quality varies a lot between self-hosted models. For self-hosted, use vLLM with a model that supports function calling well, and validate it against the evaluation set (section 13).

### 3.5 Action Guard

The single enforcement point for every state-changing operation.

Checks, in order:

1. **Mode** (`SRE_MODE`):
   - `readonly` - no action is executed. The plan is returned as a proposal to the chat.
   - `autofix` - continue with the checks below.
2. **Action allowlist** - only actions defined in enabled playbooks of the catalog (section 3.9) are accepted. Unknown action types are rejected.
3. **Risk class** - taken from the playbook, editable without a rebuild:
   - `low` - executed automatically in `autofix` (examples: restart a pod, delete a stuck job, rotate logs on a host, clean a temp directory).
   - `medium` - executed automatically only if `AUTOFIX_MAX_RISK=medium`, otherwise needs approval (examples: scale a deployment within bounds, cordon a node, restart a systemd unit).
   - `high` - always requires human approval (examples: rollback a release, drain a node, change a config, anything on production databases).
4. **Scope** - target must be allowed for autofix (per cluster / namespace / host labels).
5. **Rate limit and circuit breaker** - max actions per target per hour; repeated failures disable autofix for that target and escalate.
6. **Dry-run** - server-side dry-run where available (Kubernetes), or a precheck command on hosts.

Execution contract for every action:

```
precheck -> dry-run -> execute -> verify (metric/condition) -> success
                                         \-> rollback -> escalate
```

Every decision (allowed, denied, waiting for approval) is written to the audit log with the incident id.

### 3.6 Tool Hub

- Registry of MCP servers, each with declared tools marked as `read` or `write`.
- Two entry points:
  - read path - used by HolmesGPT;
  - write path - accepted only from the Action Guard (mTLS identity check).
- Routes each call to a target:
  - local cluster - executed directly with the hub's in-cluster read-only ServiceAccount (write tools use a separate ServiceAccount);
  - remote cluster - forwarded to the target's k8s-agent over its tunnel;
  - host - forwarded to the host-agent.
- Obtains short-lived credentials from Vault.
- Enforces output limits (bytes, lines, time window) and secret redaction before results go back to an LLM.

Initial MCP servers:

| Server | Read tools | Write tools |
|---|---|---|
| kubernetes | get/describe/list, events, pod logs (incl. `--previous`), top | restart, scale, delete pod, cordon, rollout undo |
| prometheus | instant/range PromQL, alerts, targets | - |
| logs-loki | LogQL queries with limits | - |
| logs-victorialogs | LogsQL queries with limits | - |
| logs-elasticsearch | Query DSL / ES\|QL search with limits; Elasticsearch and OpenSearch (OpenSearch also PPL) | - |
| host | system status, `journalctl`, allowlisted log files, disk/memory/process info | allowlisted commands (restart unit, clean path) |

Additional backends (cloud logging APIs, ClickHouse-based stores, etc.) are added as new MCP servers without changes to the core. Log backends are described in section 4.

### 3.7 Chat Gateway

- Internal message model:
  - outbound: `notify`, `rca`, `approval_request`, `action_result`;
  - inbound: `ask` (free-form question), `approve`, `reject`, `command` (e.g. `/investigate <target>`, `/mode`).
- Approvals: the community edition decides them in the web console only; approval requests in
  chat link to it and chat decisions are refused. The controller flag `Engine.chat_approvals`
  (set by the enterprise extension with the `chat-approvals` license feature) adds the
  Approve/Reject buttons and accepts decisions from chat adapters.
- Adapters are plugins implementing one interface:

```
Adapter
  name() -> string
  start(inbound_callback)
  send(message) -> message_ref
  update(message_ref, message)
```

- Enabled via `CHAT_ADAPTERS=telegram` (Telegram is the first adapter; Slack and others follow). Each adapter has its own config block and secret.
- Interactive buttons are mapped to `approve` / `reject` events with the approval id.
- Approvals require the user to be in an allowed group (per adapter user mapping).

### 3.8 Remote agents

**k8s-agent** (one per remote cluster):

- Deployed with a Helm chart, runs with a read-only ClusterRole and an optional separate write role (bound only if autofix is enabled for that cluster).
- Opens an outbound WebSocket tunnel to the Tool Hub (agent token, or mTLS). Works behind NAT.
- Executes Kubernetes, Prometheus and log-backend tool calls locally against that cluster's endpoints.

**host-agent** (one per standalone host):

- Single static binary as a systemd service.
- Outbound WebSocket tunnel to the Tool Hub (agent token, or mTLS).
- Executes only allowlisted commands and reads only allowlisted paths and units. No shell.
- `node_exporter` covers metrics; the agent covers logs and system state.

Both agents:

- identify themselves with a credential issued per target at enrollment: an agent token
  (default) or a client certificate;
- are registered in the Tool Hub with labels (`env`, `cluster`, `host` plus any user-defined labels) used for scoping and policy;
- never share credentials across targets.

### 3.9 Playbook catalog (configurable at runtime)

Playbooks, their risk classes and limits are an editable catalog. Changing the list does not require rebuilding or redeploying images.

Two layers:

| Layer | Where it lives | How to change |
|---|---|---|
| **Primitives** - the low-level operations the system can perform (`k8s.pod.delete`, `k8s.deployment.scale`, `host.systemd.restart`, ...) | Code (Tool Hub MCP servers, agents) | New release. Each primitive has a typed parameter schema and built-in safety bounds. |
| **Playbooks** - which primitive to run for which problem, with what parameters, risk class, precheck, verify, rollback and limits | YAML catalog | Edit the files; picked up at runtime |

This keeps the attack surface fixed (only reviewed primitives exist) while the behavior stays configurable.

Catalog source:

- Phase 1-3: YAML files in a ConfigMap (rendered from Helm values, or managed by GitOps), mounted into the Action Guard and controller at `PLAYBOOKS_PATH`. The default catalog ships with the chart in `playbooks/default/`; users can disable, override or add playbooks.
- Later (optional): a `Playbook` CRD with the same schema, for `kubectl`-native management and per-namespace ownership.

Reload behavior:

1. The services watch `PLAYBOOKS_PATH` (and `POLICIES_PATH`).
2. On change, the whole catalog is parsed and validated: schema, known primitives, parameter schemas, template variables, unique names.
3. Valid catalog - swapped atomically; the new catalog version (content hash) is logged.
4. Invalid catalog - rejected as a whole, the last valid version stays active, an error metric is raised and a notification is sent to the chat.
5. Every executed action records the playbook name and catalog version in the audit log.

Chat commands: `/playbooks` lists the active catalog and version; `/playbook <name>` shows one entry.

Playbook format:

```yaml
apiVersion: ai-sre.io/v1
kind: Playbook
metadata:
  name: restart-crashlooping-pod
spec:
  enabled: true
  description: Delete a crash-looping pod so its controller recreates it
  risk: low                      # low | medium | high
  match:                         # when the playbook applies
    alertnames: [KubePodCrashLooping]
    targetKind: k8s.pod
    labels:                      # optional scope restriction
      env: [dev, staging]
  action:
    primitive: k8s.pod.delete
    params:
      namespace: "{{ target.namespace }}"
      pod: "{{ target.pod }}"
  precheck:
    - primitive: k8s.pod.ownerExists   # never delete a bare pod
  verify:
    promql: >
      increase(kube_pod_container_status_restarts_total{namespace="{{ target.namespace }}",pod=~"{{ target.workload }}-.*"}[5m]) == 0
    timeout: 10m
  rollback: none
  limits:
    maxPerTargetPerHour: 3
```

Rules:

- Templates support only variable substitution from the incident target and alert labels. No expressions, no code execution.
- Parameters are validated against the primitive schema after substitution; primitives also enforce their own hard bounds (for example, `k8s.deployment.scale` cannot go above the configured max replicas or to zero).
- The risk class in a playbook can be raised freely; lowering it below the primitive's minimum risk is rejected (for example, `k8s.node.drain` is at least `high`).
- `SRE_MODE`, `AUTOFIX_MAX_RISK` and policies still apply on top of the catalog: a playbook never bypasses the mode.

Default catalog (shipped, all can be disabled):

| Playbook | Primitive | Risk |
|---|---|---|
| restart-crashlooping-pod | `k8s.pod.delete` | low |
| delete-failed-job | `k8s.job.delete` | low |
| restart-oom-deployment | `k8s.deployment.restart` | medium |
| scale-up-saturated-deployment | `k8s.deployment.scale` | medium |
| cordon-unhealthy-node | `k8s.node.cordon` | medium |
| rollback-failed-rollout | `k8s.rollout.undo` | high |
| restart-failed-systemd-unit | `host.systemd.restart` | medium |
| clean-host-disk | `host.path.clean` | low |

The same approach applies to host-agents: the allowlist of host primitives and paths is a local config file (`/etc/ai-sre/host-agent.yaml`), reloaded on change or `SIGHUP`. It is the last line of defense: even if the central catalog allows an action, the host-agent refuses anything outside its local allowlist.

### 3.9.1 Phase 2 implementation notes (Tool Hub and agents)

- Transport: WebSocket over TLS with JSON frames (`hello`, `request`, `response`),
  instead of gRPC (decision D15). Agents dial out; the hub never connects to them.
- Agent identity (decision D22, `toolHub.tunnel.auth`): `token` (default) - a bearer
  agent token issued at enrollment, stored hashed in `agent_tokens`, mapped to the agent
  name by the hub; TLS may end at an ingress (`toolHub.tunnel.ingress`, hub port 8081,
  only `/v1/tunnel` and `/v1/enroll` exposed). `mtls` - the client certificate
  CommonName; TLS must reach the hub's port 8443 (Service or TLS passthrough). `both`
  accepts either. The hub CA lives in the Secret `<release>-tool-hub-ca` (kept across
  upgrades); the local agent gets a static token from the chart (or a certificate in
  `mtls` mode).
- All execution happens in agents, including the control cluster itself (`localAgent`).
  The hub only routes, redacts secrets, truncates output (`toolHub.maxOutputBytes`),
  logs every call and exposes metrics.
- HolmesGPT reaches the hub as the MCP server `ai-sre-tools` (streamable HTTP, bearer
  token). The whole tool catalog is registered at startup with a `target` argument, so the
  tool list does not depend on which agents are connected; `list_targets` shows them.
- The controller tells Holmes to use the hub with `target=<cluster>` when an incident is
  not in the local cluster, and `target=<host>` for host targets.
- Write tools for remediation (phase 3) are not exposed yet.

### 3.9.2 Phase 3 implementation notes (remediation executor)

- Write primitives run in agents as `action:<primitive>` tools and prechecks as
  `check:<primitive>`; agents offer them only with `write.enabled` (k8s-agent also
  gets a separate write ClusterRole: delete pods/jobs, patch/scale deployments, patch
  nodes). `k8s.node.drain` is not automated.
- The hub exposes them only on `POST /v1/actions`, and verification reads on
  `POST /v1/query`, both with `TOOL_HUB_ACTION_TOKEN` (held by the guard, never by
  HolmesGPT); MCP never lists them.
- `POST /v1/execute` on the guard re-evaluates, then runs: prechecks -> dry run
  (Kubernetes server-side `dryRun=All`) -> action -> verify (PromQL polled every
  `VERIFY_POLL_INTERVAL` until the playbook timeout, max 20 min) -> rollback when
  verification fails. Outcomes: succeeded, refused, failed, rolled_back, rollback_failed.
- A precheck without params inherits the action params its primitive declares.
- Every execution counts against the rate limit. Counts live in the NATS KV bucket
  `aisre_guard_actions` (one key per target, entries expire after 1 h), so they survive
  restarts and are shared by replicas; while NATS is unavailable the guard uses its
  in-memory counts (metric `aisre_guard_rate_limit_store_errors_total`).
- The controller moves the incident to
  `remediated` or `remediation_failed` and posts every step to chat.

### 3.10 Knowledge base (skills)

Troubleshooting knowledge is kept as HolmesGPT skills in `skills/<name>/SKILL.md`:
YAML frontmatter with `name` and `description`, then a procedural markdown body
(workflow, how to synthesize findings, remediation and verification).

- Holmes lists the skills by `description` and loads a matching one with its
  `fetch_skill` tool before investigating, so the guide shapes the tool calls.
- The chart ships the skills in the ConfigMap `<release>-skills`; an init container
  in the Holmes pod lays them out as `<name>/SKILL.md` (ConfigMap keys cannot contain
  directories). Installations add or override skills with `knowledge.extraSkills`.
- Holmes reads skills at startup, so a change needs a Holmes restart.
- Skills tie diagnosis to the playbook catalog: they state when a restart does not
  help and which playbook fits (`remediation-selection`).

Default skills: pod-crashloop, container-oomkilled, pod-pending-scheduling,
image-pull-failure, deployment-rollout-stuck, node-not-ready, pvc-storage-issues,
service-errors-latency, certificate-expiry, host-systemd-unit-failed, host-disk-full,
remediation-selection.

### 3.11 Web console

`services/web`, FastAPI with server-rendered pages (Jinja2, htmx; no frontend build).

- Reads incidents, events and approvals from PostgreSQL; guard status and catalog
  from the Action Guard API; playbook YAML and skills from the same ConfigMaps the
  other services use; connected agents from the Tool Hub `GET /v1/targets` (read token,
  never the action token); the agent registry from the table `agents`, written by the hub.
- Acts only through existing paths: approvals and `/reinvestigate` go to ingest as
  the chat adapter `web`; questions to HolmesGPT go the same way and the answers come
  back on `notify.chat` (durable consumer `web`, `reply_to.adapter == "web"`).
- Own tables: `web_users`, `web_audit`, `web_questions`.
- Authentication: local accounts (bcrypt, login throttling) and OIDC authorization
  code flow with PKCE (Authlib). OIDC roles come from a claim (`groups` by default,
  dotted paths such as `realm_access.roles` work) mapped to viewer, operator or admin;
  the role is refreshed at every sign-in. Disabling local authentication keeps local
  accounts in the database but blocks their sign-in; at least one method must be on.
- Sessions: signed cookie, SameSite=Lax, CSRF token on every state-changing request;
  the user is re-read from the database on each request, so disabling takes effect
  immediately. The last active admin cannot be demoted or disabled.

## 4. Logs access

Logs are available as read tools in both modes:

| Source | Access |
|---|---|
| Centralized logs | One of the supported log backends (below) |
| Pod logs, local cluster | `kubernetes` MCP server (`logs`, `logs --previous`, events) |
| Pod logs, remote cluster | Same tools, executed by the k8s-agent; the cluster's own log backend if present |
| Hosts | `journalctl -u <unit>` and allowlisted files via the host-agent |

### 4.1 Supported log backends

| Backend | MCP server | Native query language | Notes |
|---|---|---|---|
| Grafana Loki | `logs-loki` | LogQL | Label selectors + line filters |
| VictoriaLogs | `logs-victorialogs` | LogsQL | Stream filters + word/phrase filters; fast full-text search |
| Elasticsearch | `logs-elasticsearch` | Query DSL, ES\|QL | Index pattern per source |
| OpenSearch | `logs-elasticsearch` (OpenSearch mode) | Query DSL, PPL | Same server, API flavor selected in config |

Elasticsearch and OpenSearch share one MCP server: the search API is largely compatible, the differences (auth, ES|QL vs PPL, version-specific endpoints) are handled by a `flavor: elasticsearch | opensearch` setting.

### 4.2 Unified logs interface

Every log backend MCP server exposes the same two read tools, so the investigator and playbooks do not depend on the backend:

| Tool | Purpose |
|---|---|
| `logs.query` | Backend-neutral search: `selector` (labels such as cluster, namespace, workload, pod, container, host, unit), `contains` / `regex` filters, `level`, `start` / `end`, `limit` |
| `logs.native` | Pass-through of a native query (LogQL, LogsQL, Query DSL, ES\|QL, PPL) for cases the neutral form cannot express; still subject to the same limits |

Additional optional tools: `logs.stats` (counts by level or pattern over time, useful to find when an error started) and `logs.context` (lines around a given log entry).

Each backend server maps the neutral selector to its own fields through a configurable field mapping, because label and field names differ between installations:

```yaml
logBackends:
  - name: main-vl
    type: victorialogs
    url: http://victorialogs.monitoring:9428
    scope: { cluster: prod-1 }
    fields:
      namespace: kubernetes.pod_namespace
      pod: kubernetes.pod_name
      container: kubernetes.container_name
      level: level
  - name: legacy-es
    type: elasticsearch
    flavor: opensearch
    url: https://opensearch.logging:9200
    index: "logs-*"
    scope: { cluster: legacy }
    fields:
      namespace: kubernetes.namespace_name
      pod: kubernetes.pod_name
      message: message
      level: log.level
```

- `scope` binds a backend to targets, so the Tool Hub picks the right backend (and the right agent tunnel) for an incident. Several backends can be active at once.
- Backend list and field mappings are runtime configuration (`LOG_BACKENDS_PATH`), reloaded like the playbook catalog.
- Credentials (basic auth, API keys, client certificates) come from Vault.

### 4.3 Constraints applied by the Tool Hub

- mandatory time window and line limit;
- server-side filtering first (backend query filters, `grep` patterns on hosts);
- query cost guard: rejected or narrowed if the time range or index pattern is too wide;
- collapsing of repeated lines before returning to the LLM;
- secret and PII redaction.

## 5. Data flows

### 5.1 Event envelope

```json
{
  "id": "evt_...",
  "kind": "alert | check | chat_command | approval",
  "source": "alertmanager | scheduler | chat:<adapter>",
  "target": { "cluster": "...", "namespace": "...", "host": "...", "labels": { "env": "..." } },
  "fingerprint": "...",
  "incident_key": "...",
  "payload": { },
  "received_at": "2026-10-11T00:00:00Z"
}
```

### 5.2 Alert investigation

1. Alertmanager sends a webhook to Event Ingest.
2. Ingest deduplicates, correlates, enriches and publishes to `events.alerts`.
3. The controller creates an incident and calls HolmesGPT.
4. HolmesGPT queries metrics, logs, events and host state through the Tool Hub, using the LLM Gateway.
5. HolmesGPT returns the RCA and a remediation proposal.
6. The controller stores the result and sends the RCA to the Chat Gateway.

### 5.3 Remediation

1. The controller maps the proposal to a playbook and builds an action plan.
2. The Action Guard evaluates the plan:
   - `readonly` - returned as a proposal; the chat shows the suggested action without an execute button;
   - `autofix`, low risk - executed;
   - `autofix`, above the allowed risk - an approval request is sent to the chat.
3. Approval from the chat goes back through Ingest to the controller, then to the Guard.
4. The Guard executes through the Tool Hub write path, verifies and rolls back on failure.
5. The result is posted to the chat and the incident is resolved or escalated.

### 5.4 Scheduled checks

The scheduler posts a check task to Ingest. It follows the same pipeline as an alert, with `kind=check`. No notification is sent if nothing is found, unless configured.

### 5.5 Chat questions

`/investigate <target>` or a free-form question creates an ad-hoc incident with `kind=chat_command` and goes through the same pipeline. The answer is posted to the same thread.

## 6. Configuration

| Variable | Values | Default | Used by |
|---|---|---|---|
| `SRE_MODE` | `readonly`, `autofix` | `readonly` | Action Guard, controller |
| `AUTOFIX_MAX_RISK` | `low`, `medium` | `low` | Action Guard |
| `AUTOFIX_MAX_ACTIONS_PER_HOUR` | integer | `5` | Action Guard (per target) |
| `AUTOFIX_SCOPE` | label selector | empty (none) | Action Guard |
| `LLM_PROVIDER` | `claude`, `openai`, `ollama` | `claude` | LLM Gateway |
| `LLM_MODEL` | model id | provider default | LLM Gateway |
| `LLM_FALLBACK` | ordered list | empty | LLM Gateway |
| `LLM_SENSITIVE_PROVIDER` | provider | `ollama` | LLM Gateway routing |
| `CHAT_ADAPTERS` | comma-separated list | `telegram` | Chat Gateway |
| `PLAYBOOKS_PATH` | directory | `/etc/ai-sre/playbooks` | Action Guard, controller |
| `POLICIES_PATH` | directory | `/etc/ai-sre/policies` | Action Guard |
| `LOG_BACKENDS_PATH` | file | `/etc/ai-sre/log-backends.yaml` | Tool Hub, agents |
| `HOLMES_URL` | URL | in-cluster service | controller |
| `NATS_URL` | URL | in-cluster service | all services |
| `DATABASE_URL` | URL | from secret | controller, Guard |

Secrets (LLM keys, chat tokens, database credentials) come from Vault or External Secrets, never from plain ConfigMaps.

Mode can also be overridden per target scope (for example, `autofix` for `env=dev`, `readonly` for `env=prod`) through the policy file; the environment variable is the global default and the upper bound.

## 7. Security model

- **Least privilege.** Separate ServiceAccounts and RBAC for read and write tools, in the control cluster and in every agent.
- **No credential sharing.** Each cluster and host has its own agent identity and credential.
- **Authenticated, encrypted tunnels** between Tool Hub and agents (agent tokens over TLS, or mTLS); agents dial out only.
- **No shell.** Host actions are allowlisted commands with validated parameters.
- **LLM output is untrusted.** Only structured, schema-validated proposals mapped to known playbooks can become actions.
- **Prompt injection.** Logs and alert text are data. Tool results are wrapped and marked as untrusted in prompts; write tools are never exposed to the investigator.
- **Data egress.** Redaction before cloud LLM calls; sensitive scopes routed to the self-hosted model.
- **Approvals** are bound to an authorized chat user and an approval id with expiry.
- **Audit** of every LLM call, tool call, decision and action, linked to the incident id.

## 8. Deployment

- Control plane: Helm chart into namespace `ai-sre` of the SRE control cluster.
- Stateful dependencies: NATS JetStream, PostgreSQL (with pgvector), optional in-cluster Ollama/vLLM on a GPU node.
- Remote clusters: `k8s-agent` Helm chart.
- Hosts: `host-agent` package (deb/rpm) or binary + systemd unit, deployable with Ansible.
- High availability: stateless services run with 2+ replicas; the controller uses leader election or queue consumer groups for incident ownership.

## 9. Observability of the harness

- Prometheus metrics from every service: events received, incidents by state, investigation duration, LLM tokens and cost per provider, tool-call latency and errors, actions executed/denied/rolled back.
- OpenTelemetry traces across ingest -> controller -> Holmes -> Tool Hub -> agent.
- Langfuse for LLM traces and prompt/response inspection.
- Alerts on the harness itself: queue lag, agent disconnected, LLM provider errors, autofix circuit breaker open.

## 10. Proposed repository layout

```
ai-sre/
  ARCHITECTURE.md
  charts/
    ai-sre/                 # control plane chart
    k8s-agent/
  services/
    ingest/
    controller/
    action-guard/
    tool-hub/
    chat-gateway/
      adapters/
        slack/
        telegram/
  agents/
    k8s-agent/
    host-agent/
  mcp-servers/
    kubernetes/
    prometheus/
    logs-loki/
    logs-victorialogs/
    logs-elasticsearch/       # Elasticsearch and OpenSearch
    host/
  playbooks/                # default playbook catalog (shipped with the chart, overridable)
    default/
  policies/                 # scope and mode policies
  skills/                   # knowledge base: troubleshooting skills for HolmesGPT
  services/web/             # web console
  evals/                    # incident replay scenarios for LLM evaluation
```

Proposed languages (to be confirmed):

- Go - agents, Tool Hub, Action Guard (static binaries, low footprint, good gRPC/mTLS story);
- Python - controller and chat gateway (closer to the HolmesGPT and LLM ecosystem).

## 11. Delivery phases

Status as of 2026-10-11. This list is the single source for what is done and what remains.

1. **Foundation** - DONE. Ingest, queue, controller, HolmesGPT, LLM Gateway, Chat Gateway
   (Telegram, console), knowledge base (skills), playbook catalog. Added on the way: web
   console with local and OIDC sign-in (section 3.11).
2. **Reach** - DONE except Vault. Tool Hub, k8s-agent, host-agent, log backends
   (Loki, VictoriaLogs, Elasticsearch/OpenSearch), output limits and redaction.
   - TODO: Vault integration for tool and agent credentials (today: Kubernetes Secrets and
     files; agent certificates from the hub CA).
3. **Remediation** - DONE except scopes. Action Guard, risk classes, approvals, executor
   with dry run, verify and rollback, opt-in write access per agent.
   - TODO: scope policies (`policies/`, `POLICIES_PATH`): mode and max risk per label scope,
     e.g. autofix for `env=dev`, readonly for `env=prod`. Today the mode is global.
4. **Quality** - IN PROGRESS.
   - DONE: similar past incidents by full-text search (D20): hints in the controller prompt
     and a "Similar incidents" card on the web console incident page (marks the ones used
     in the last investigation); one shared query in `aisre_common.similar`.
   - DONE: persistent guard rate limits in NATS KV (D21).
   - DONE: no emoji in LLM output (rule in every controller system prompt).
   - DONE: short chat report: the model returns `chat_report` (at most 8 lines: symptom,
     root cause, evidence, fix) for chat; the full analysis stays in the web console and
     the Telegram footer links to it (`WEB_BASE_URL`).
   - DONE: web console page "Targets": agents connected to the Tool Hub (labels, read
     tools, write actions, version, incident counts) and targets with recent incidents but
     no agent. The console gets the hub read token only.
   - DONE: evaluation set `evals/` (8 cases, runner `evals/run.py`, section 13). First
     run on Claude: 7/8 (bad-rollout picks the rollback only in some runs).
   - TODO, in order:
     5. Provider comparison: run the evaluation set per `llm.provider` and record results.
        DONE (`evals/results/COMPARISON.md`): Claude Sonnet 5.5 24/24, Qwen3-8B on vLLM
        5/16, Qwen3-8B on Ollama 0/8. 8B models stay fallbacks only.
     6. Autofix for selected production scopes (needs the scope policies from phase 3).
     7. Controller HA (leader election), or document the single-replica limitation.
     8. Embeddings (pgvector) for similar incidents once an embedding model is configured.
5. **Fleet management** - DONE (requested and built 2026-10-11). Before it, agents were enrolled by hand:
   `scripts/issue-agent-cert.sh` signs a client certificate with the hub CA key read from
   the cluster, and the hub only knows agents while they are connected.
   - DONE: items 3 and 4 and the `enroll` part of item 5: join tokens (UI and
     `POST /api/v1/agents/enrollments`, one-time, max TTL 7 days, the name of a known agent
     needs `replace`, a new token expires earlier unused ones of the name), `GET /v1/ca` + `POST /v1/enroll` on the hub tunnel port (CA pinned by
     SHA-256, CSR signed by the hub, the CA key is mounted only into the hub, failed
     tokens throttled per IP), revocation (UI and API; serials in `agent_certs`, the hub
     refreshes them every 30 s, drops sessions and refuses the certificate), admin-issued
     API tokens (page "API tokens", scopes `agents:read|enroll|revoke`, hashed, expiry,
     last use, audit as `token:<name>`).
   - DONE: item 5: Ansible role `deploy/ansible/roles/ai_sre_host_agent` (API call on the
     controller, join token through the environment, idempotent), and enrollment for the
     k8s-agent chart (`enrollment.token` + `caSha256`: an init container enrolls once
     and keeps the certificate in the Secret `<release>-tls`; later restarts reuse it).
   - DONE: items 1 and 2: the hub writes table `agents`
     (`pkg/agentstore`; connect/disconnect through one writer goroutine, heartbeat
     every minute, all marked offline at hub start; single hub replica), and the admin
     page "Agents" shows online/stale/offline, labels, access, version, certificate
     expiry and lets admins forget offline agents (audited).
   1. Agent registry in PostgreSQL: every enrolled agent (name, kind, labels, certificate
      serial and expiry, enrolled by/at, last seen, last version, revoked). The hub reports
      connects and disconnects, so the console shows offline agents too.
   2. Admin section "Agents" in the web console: state of every agent (online/offline,
      last seen, version, tools, write access, certificate expiry), revoke, and enroll a
      new agent (mainly standalone hosts that are not Kubernetes nodes). Enrollment
      produces a one-time join token and a ready install command.
   3. Enrollment API: `POST /v1/agents` (create, returns a one-time join token),
      `GET /v1/agents`, `DELETE /v1/agents/{name}` (revoke). The agent exchanges the join
      token for a certificate (CSR signed by the hub; the CA key stays in the hub), so no
      key material is copied by hand. Revoked serials are refused at the tunnel.
   4. API tokens for automation, issued in the console by admins only: named, scoped
      (e.g. `agents:enroll`, `agents:read`), shown once, stored hashed, with expiry and
      last use; every use is audited. Used by Ansible/cloud-init to enroll standalone
      hosts unattended.
   5. Install path for hosts: host-agent package or binary + systemd unit with an
      `enroll --hub <url> --token <join token>` step; an Ansible role example.

6. **Agent tokens** - DONE (2026-10-12). TLS passthrough of the tunnel through a Gateway
   took down the Cilium Envoy of a production cluster (memory), so agents authenticate with a
   bearer token by default and the tunnel can sit behind a normal ingress (D22).
   - Enrollment without a CSR returns an agent token (`aisre-agent-...`, hashed in
     `agent_tokens`; re-enrollment revokes the earlier tokens of the name). The CA pin is
     optional in token mode (only for the hub's own TLS port).
   - The hub checks tokens against the database (the last loaded list while it is down),
     refreshes active tokens with the revoked serials every 30 s and drops sessions of
     revoked tokens. Static tokens from `STATIC_TOKENS_DIR` (the chart's local agent).
   - `toolHub.tunnel.auth: token|mtls|both`, `toolHub.tunnel.ingress`; agents: `auth`,
     `tokenFile`, `tls.ca` optional (system roots); k8s-agent chart `auth`, Secret
     `<release>-token`; Ansible `ai_sre_agent_auth`, `ai_sre_hub_ca_pinned`; console
     shows the credential and revokes tokens and certificates together.
   - The cluster agent was renamed to k8s-agent (binary, image, chart).
7. **CLI `aisre`** - DONE (2026-10-12). Read-only terminal client of the console API:
   `status` (mode, LLM, incidents, approvals, component health; exit 2 when unhealthy),
   `incidents`, `incident <id prefix>`, `approvals`, `agents`, `-o json`. Console API
   `/api/v1/status|incidents|approvals` with the token scope `status:read`. Binaries for
   linux and darwin (amd64, arm64): release assets, OCI `aisre-bin`, CI workflow artifact.

8. **Open core** - IN PROGRESS (2026-10-13). Community edition under Apache-2.0, enterprise
   features in separate modules: autofix above risk low and approvals in chat, more than 5
   agents, audit export, later SLO and postmortems; SSO stays open.
   - DONE: kind demo `deploy/demo` (`make demo`, CI smoke test with a mock LLM); shared Go
     packages in `pkg/`; Action Guard as package `guard` with the `Decider` interface
     (`CommunityDecider`: autofix up to risk low); Python extension entry points
     `ventrolab.aisre.web` and `ventrolab.aisre.controller`.
   - TODO: chart `edition`, `imagePullSecrets`, `license.existingSecret`.

The Action Guard and the read/write separation are part of the design from phase 1, so enabling autofix later is a configuration change, not a refactoring.

## 12. Open questions

- Self-hosted model choice and GPU availability.
- Playbook catalog source for phase 3+: stay with ConfigMap files or move to a `Playbook` CRD (section 3.9).

## 13. Evaluation

- `evals/` holds live cases: manifests that really break a workload, the alert, and the
  expected root cause keywords, playbook and minimum confidence. `evals/run.py` sets them
  up in fresh namespaces, sends the alerts through ingest and scores the incidents
  (details: `evals/README.md`).
- Evaluation alerts carry the label `aisre_eval`: the controller investigates them as
  usual but sends nothing to chat, never remediates, and keeps them out of the similar
  incidents in both directions, so runs are independent.
- Every change of model, provider, prompt or skill is run against the set.
- Metrics: RCA correctness, correct action selection, confidence, tool-call count,
  latency. Cost is not measured yet.
- Replaying recorded tool responses (no cluster needed) is a possible later addition.
