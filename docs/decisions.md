# Decisions

Short records of choices made, newest last. Change a decision by adding a new entry.

## D1. HolmesGPT is the investigator, not our own agent loop
Mature read-only toolsets (Kubernetes, logs, Prometheus), skills, MCP support. Our
code owns everything around it: incident lifecycle, remediation, integrations.
Holmes never receives write tools.

## D2. LLM access through LiteLLM with an OpenAI-compatible endpoint
Provider is configuration (`llm.provider`, `llm.fallbacks`). Holmes always talks to the
model `default`. Per-model `additional_drop_params` absorb provider quirks.

## D3. Claude for investigations, small self-hosted models only as fallback
Tested: qwen3:8b (Ollama, vLLM) cannot drive Holmes' tool loop; Claude finds the root
cause in 30-60 s. A bigger self-hosted model does not fit the shared 20 GiB GPU.

## D4. Read and write paths are separate; Action Guard is the only gate for writes
`SRE_MODE=readonly|autofix` is configuration. In readonly nothing is executed. High risk
always needs approval. Execute re-evaluates the request (mode or catalog may have changed).

## D5. Playbooks are data, primitives are code
Primitives (typed params, minimum risk) are reviewed code; playbooks are YAML loaded at
runtime with whole-catalog validation. Templates allow variable substitution only.
The LLM may only pick a playbook name from the offered list; parameters come from
templates, never from the model.

## D6. Knowledge base = HolmesGPT skills
`skills/<name>/SKILL.md`, matched by description and fetched by Holmes itself. Chosen
over our own RAG for now: zero extra infrastructure and it steers the tool calls.
Retrieval of similar past incidents (pgvector) stays in phase 4.

## D7. NATS JetStream between ingest, controller and chat gateway
Durability across restarts and Alertmanager deduplication via `Nats-Msg-Id`.

## D8. One controller replica owns incident state
Simplicity first; HA via leader election later.

## D9. Python for services close to Holmes and LLMs, Go for the guard and agents
Guard and future agents need small static binaries and a strict trust boundary.

## D10. Chat gateway with adapters; Telegram first
One internal message model (notify, ask, approve, reject). Forum topics supported via
`TELEGRAM_THREAD_ID`. Only users in `TELEGRAM_APPROVERS` can press Approve/Reject.

## D11. A small model's late conclusions are capped
If the main pass has no structured verdict, the follow-up pass result gets confidence
<= 0.3 and no playbook: an invented confident RCA is worse than "unknown".

## D12. Product stays company-agnostic
No company names, hosts or chat ids in the repository. Environment specifics live in the
deployment's own values and Kubernetes Secrets; the repository ships only the generic kind
demo (`deploy/demo`).

## D13. Web console: server-rendered FastAPI, actions through ingest
Jinja2 + htmx instead of a SPA: no Node build, same language and image pattern as the
other services, enough interactivity for an admin console. The console never writes
controller tables: approvals, reinvestigations and questions go through ingest like
chat commands, so rules, deduplication and timelines stay in one place.

## D14. Console authentication: local and OIDC, roles from IdP groups
Local accounts for bootstrap and small installations; OIDC (Keycloak first, any
standard provider) for teams. SSO roles are re-read at every sign-in from a
configurable claim; no group means no access by default. Local sign-in can be turned
off entirely once SSO works.

## D15. Agent tunnel: WebSocket over TLS and JSON frames
Instead of gRPC: no code generation, simple request multiplexing by id, passes through
L7 proxies and ingress controllers that support WebSocket. Revisit if throughput or
strict schemas become a problem. Authentication: D22.

## D16. Every tool runs in an agent, the hub only routes
The control cluster has its own k8s-agent like any remote cluster. The hub holds
no cluster credentials; it authenticates agents, applies limits and redaction, and
audits calls. Losing the hub never exposes cluster access.

## D17. Static MCP tool catalog with a target argument
All tools are registered at hub startup; agents advertise which ones they serve. HolmesGPT
loads tools once at startup, so a dynamic list would miss agents that connect later.

## D18. Write access is opt-in per agent and separated by token
Agents serve write primitives only with `write.enabled`, with separate RBAC. The hub's
write path uses a token only the Action Guard has; HolmesGPT's MCP token cannot reach it.
A deployment can therefore run autofix in one cluster and stay read-only in another.

## D19. Execution is synchronous inside the guard, asynchronous for the controller
`/v1/execute` returns after verification (minutes); the controller awaits it in a
background task, so the event consumer never blocks.

## D20. Similar incidents by full-text search first, embeddings later
No embedding model is available in the reference setup (Anthropic has no embeddings API; the
self-hosted models are chat models). Postgres full-text search over title, root cause
and alert names finds repeated incidents well enough and needs no new infrastructure.
Switch to pgvector when an embedding model is configured in the LLM gateway; keep the
same `Repo.similar` interface.

## D21. Guard rate limits in NATS KV, memory as fallback
NATS JetStream is already required, so a KV bucket (`aisre_guard_actions`, TTL 1 h) adds
no infrastructure. One key per target with a JSON list of timestamps, written with
compare-and-set, keeps replicas consistent. The guard must keep working when NATS is
down: every action is also counted in memory, the larger count wins, and the bucket is
opened lazily (retry every 30 s), so the guard starts before NATS.

## D22. Agents authenticate with a bearer token by default, mutual TLS is optional
Mutual TLS needs the TLS session to reach the hub, so a remote endpoint needs its own
Service/port or TLS passthrough. Passthrough through a Gateway (Cilium Envoy) exhausted
the proxy's memory and took a cluster down. An agent token in the `Authorization` header
lets TLS end at an ordinary ingress with a public certificate, like the web console.
The enrollment flow stays (one-time join token -> credential); the hub stores only the
token hash and maps it to the agent name itself, so an agent cannot pick its name.
Trade-offs: the ingress sees the token and the tunnel traffic in clear text (same
cluster), and a leaked token can be replayed until revoked - like a leaked private key,
which lives on the same disk. Revocation is immediate on the next refresh (30 s). mTLS
stays available (`toolHub.tunnel.auth: mtls|both`) for setups that need it.
