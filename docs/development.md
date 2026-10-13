# Development guide

How the system is built, run and tested, and the non-obvious problems found so far.

## 1. Local environment

The demo in `deploy/demo` is also the development environment: one kind node with
VictoriaMetrics, kube-state-metrics, vmalert with Alertmanager, Ventrolab AI SRE and broken
workloads.

```
make -C deploy/demo demo DEV=1                    # build images from this checkout, load them into kind
make -C deploy/demo demo DEV=1 LLM_PROVIDER=mock  # no LLM key: a canned RCA, for plumbing tests
make -C deploy/demo images install DEV=1          # after a code change: rebuild, reload, upgrade
kubectl -n ai-sre rollout restart deploy           # the tag stays "dev": restart to pick up new images
make -C deploy/demo status                        # incidents and their root causes
make -C deploy/demo clean
```

kind writes its context into `KUBECONFIG` (default `~/.kube/config`) and switches the current
context; point `KUBECONFIG` at a separate file if that file holds other clusters.

```
kind cluster (context kind-ventrolab-demo)
  ns monitoring
    vmsingle-victoria-metrics-single-server :8428
    kube-state-metrics :8080 (scraped by a dedicated job with honor_labels)
    vmalert-victoria-metrics-alert-server :8880, -alertmanager :9093 -> ingest webhook
  ns ai-sre (release "ai-sre")
    ingest :8080, action-guard :8080, controller (metrics :9090), chat-gateway (metrics :9090)
    holmes :80, litellm :4000, nats :4222, postgres :5432, tool-hub :8080/:8443/:8081
    local-agent (k8s-agent of this cluster), web :8080 (NodePort 30080 -> localhost:8088)
  ns shop
    checkout (crash loop: missing config), cart (OOM killed), nightly-report (failing job)
```

## 2. Build and deploy

- `make images` builds all images as `$(REGISTRY_PUSH)/<name>:<git sha>-<time>`, pushes
  them and writes the tag to `.last-tag`; `make images-build` builds without pushing.
- `make deploy VALUES=<file>` installs the chart from this checkout with the last pushed tag
  into the current kube context and restarts Holmes (it reads skills only at startup).
- Python images: build context is the repo root (`libs/` is copied in). Go images:
  `build/go.Dockerfile` (root module) and `services/action-guard/Dockerfile`; distroless,
  nonroot.
- All containers run with a read-only root FS, non-root, no capabilities.

## 3. Runtime facts

- NATS streams: `EVENTS` (`events.alert|check|chat|approval`, 7 d, 15 min duplicate window,
  dedup id `fingerprint:status:startsAt`), `NOTIFY` (`notify.chat`, 1 d).
  Durable consumers: `controller` (events), `chat-gateway` (notify, DeliverPolicy NEW).
- Incident key: `<cluster>/<namespace>/<workload>` (or node/host); all alerts about one
  workload join one open incident. Workload is derived from labels or the pod name
  (Kubernetes suffix alphabet `bcdfghjklmnpqrstvwxz2456789`).
- Postgres tables: `incidents`, `incident_events` (timeline), `approvals`; the hub owns
  `agents`, `agent_enrollments`, `agent_certs`, `agent_tokens`; the console owns users,
  sessions, `api_tokens`, `audit_log`, `questions`.
  Inspect: `kubectl -n ai-sre exec ai-sre-postgres-0 -- psql -U aisre -d aisre -c '...'`.
- Holmes API: `POST /api/chat {ask, model, additional_system_prompt, conversation_history}`
  -> `{analysis, tool_calls[{tool_name, description, result{status,error,data}}], conversation_history}`.
  Model name in Holmes is `default` (Holmes `modelList`, served by LiteLLM).
- Controller flow: investigate -> parse final ```json block -> if missing, one follow-up in
  the same conversation (`FINALIZE_PROMPT`), whose result is capped at confidence 0.3 and
  never selects a playbook -> guard evaluate -> notify.
- Guard API: `GET /v1/status`, `GET /v1/playbooks[?alertname=&target_kind=]`,
  `POST /v1/evaluate`, `POST /v1/execute`. Catalog reload: poll every 10 s, whole-catalog
  validation, an invalid catalog keeps the previous version and posts a `system`
  notification.

## 4. Problems found and their fixes (do not rediscover)

| Symptom | Cause | Fix |
|---|---|---|
| `ImagePullBackOff ... not found` right after `make images`; `.last-tag` differs from the pushed tag by one second | `TAG ?=` was recursive: every use re-ran `date` | `TAG` is evaluated once (`ifndef TAG` + `:=`) |
| Controller crashes at start with `PermissionError: Operation not permitted` | Cilium socket LB returns EPERM for a Service without ready endpoints (Postgres starting) | retry loops in `Repo.connect` and `bus.connect` |
| No kube-state-metrics series in VictoriaMetrics | the vmsingle endpoints job requires `prometheus.io/port` | dedicated `kube-state-metrics` scrape job |
| kube-state-metrics series carry the monitoring namespace | the endpoints job overwrites target labels | `honor_labels: true` on that job |
| Holmes kubectl tools fail: `request header 'accept' contains disallowed shell metacharacter(s) '*'` | Holmes passes HTTP request headers into tool templates; httpx/curl send `Accept: */*` | the controller sends `Accept: application/json`; do the same in manual curl tests |
| Claude returns 400 `temperature is deprecated for this model` | Holmes sends `temperature`; current Claude models reject sampling params | LiteLLM `additional_drop_params: [temperature, top_p]` on the claude model |
| Self-hosted models return 400 `max_tokens=64000 cannot be greater than max_model_len` | Holmes assumes 64K output | `additional_drop_params: [max_tokens]` on vllm and ollama |
| RCA written by a small model although Claude is configured | LiteLLM silently fell back (`default` -> fallbacks) | check `x-litellm-attempted-fallbacks` / LiteLLM logs; test with `"disable_fallbacks": true` |
| 8B models produce no RCA | they ignore instructions in Holmes' long context, write tool calls as text, invent causes | use Claude/OpenAI for investigations; the finalize cap prevents confident invented RCAs |
| Telegram shows raw `**` and fences | adapter escaped Markdown | `markdown_to_html` in the telegram adapter, plain-text fallback on parse errors |
| A Service named `vllm` breaks the vLLM container | service-link env vars `VLLM_PORT_8000_TCP...` collide with vLLM settings | `enableServiceLinks: false` (set on all our pods) |
| OIDC redirect/issuer mismatch | the browser and the console pod must use the same issuer URL | the IdP hostname must be reachable from both |
| Agent tunnel through Gateway API TLS passthrough exhausted Cilium Envoy memory | passthrough of long-lived WebSockets | agent tokens over a TLS-terminating ingress (decision D22) |
| Two pods of one agent replace each other's tunnel during an update | rolling update starts the new pod before the old one stops | agents deploy with `strategy: Recreate` |
| Ansible `uri` fails: `~/.netrc access too permissive` | the uri module reads the controller netrc | the role sets `use_netrc: false` |

## 5. Testing

- Unit tests: `make test`. Engine tests use in-memory fakes (`tests/test_engine.py`);
  guard tests validate the real `playbooks/default` catalog.
- End to end: `make -C deploy/demo demo DEV=1`, wait for the alerts, then
  `make -C deploy/demo status`. The fastest loop after a prompt change is
  `/reinvestigate <id>` posted to ingest `/v1/chat` (`scripts/chat.sh`).
- Reading results: Postgres `incidents`, chat-gateway logs (`[rca]` lines from the console
  adapter), Holmes logs (`Running tool #N`), `aisre incident <id>`.
- Evaluation set: `evals/` (live broken workloads, scored RCA), see `evals/README.md`.

## 6. Tool Hub and agents

- Go root module (`go.mod`, Go 1.25, image `golang:1.25.14`); `make test-go` runs both Go
  modules in Docker; `make host-agent` builds `dist/host-agent`, `make cli` builds `dist/aisre`.
- Check from Holmes: `scripts/holmes-ask.sh "Using ai-sre-tools: call list_targets ..."`.
- The hub logs every call: `kubectl -n ai-sre logs deploy/ai-sre-tool-hub | grep '"tool call"'`.
- Agents authenticate with agent tokens by default (D22); hub and agents re-read credentials
  on every connection, so rotation needs no restart.
- A second cluster for remote-agent tests: another kind cluster with the `k8s-agent` chart
  and a join token from the console (`enrollment.token`); load the image with
  `kind load docker-image` (pullPolicy Never).
- Host agent test: run `dist/host-agent -config <file>` as a normal user (adm group reads
  /var/log and the journal), then stop it.
- Holmes passes null for unset MCP arguments; agents treat null as empty.
- Executor check without an LLM: port-forward the guard and POST `/v1/execute` with an
  incident-like body and `approved_by`; the response lists every step.
- Prechecks written without params in playbooks inherit the action params their primitive
  declares (otherwise the agent gets no namespace).

## 7. Web console

- Login checks without a browser: local form post with the CSRF token from `/login`.
- Visual check: `scripts/web-screenshots.sh <dir> <path>...` (headless Chrome via the
  DevTools protocol with the admin session cookie), then look at the PNGs.
- Unit tests use `create_app(cfg, FakeStore(), FakeServices())`; no database needed.
- SSO-only mode: `--set web.auth.local.enabled=false`; the login page shows only the SSO
  button.

## 8. Chart notes

- The release name is assumed to be `ai-sre` in a few subchart values (Holmes secret name,
  LiteLLM URL, skills ConfigMap name).
- `llm.provider` selects which `llm.models` entry is served as `default`; `llm.fallbacks`
  lists the others in order.
- Generated secrets (`LITELLM_MASTER_KEY`, `INGEST_TOKEN`, `POSTGRES_PASSWORD`, tokens of the
  hub and the local agent) are kept across upgrades via `lookup`.
- `charts/ai-sre/files/playbooks` and `files/skills` are symlinks; Helm warns about them
  and includes the content.
