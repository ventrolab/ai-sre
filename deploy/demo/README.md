# Demo on kind

A local kind cluster with everything Ventrolab AI SRE needs to show what it does:

- VictoriaMetrics, kube-state-metrics, vmalert with Alertmanager (namespace `monitoring`);
  rules labeled `ai_sre="investigate"` are routed to AI SRE, as in
  [docs/alerting.md](../../docs/alerting.md);
- Ventrolab AI SRE from the released chart (namespace `ai-sre`), readonly mode, chat output
  in the console adapter;
- three broken workloads in namespace `shop`, each with a different root cause:
  `checkout` crash loops (its config file is not mounted), `cart` is OOM killed (its cache
  outgrows the memory limit), `nightly-report` fails (its database does not exist).

## Run

Needs Docker with about 8 GB of memory, [kind](https://kind.sigs.k8s.io), helm and kubectl,
and an LLM key.

```
export ANTHROPIC_API_KEY=...         # or OPENAI_API_KEY with LLM_PROVIDER=openai
make demo
```

The first run pulls the images (a few minutes). At the end it prints the console URL
(http://localhost:8088) and the admin password. Alerts fire 2-5 minutes after the
workloads start; every investigation takes about a minute.

```
make status                          # incidents with status, confidence and root cause
kubectl --context kind-ventrolab-demo -n ai-sre logs -f deploy/ai-sre-chat-gateway
make clean                           # delete the cluster
```

kind writes its context into `KUBECONFIG` (default `~/.kube/config`) and makes it the
current context. Use `export KUBECONFIG=~/.kube/ventrolab-demo` first to keep it separate.

## Options

| Variable | Default | Meaning |
|---|---|---|
| `LLM_PROVIDER` | `claude` | `claude` (ANTHROPIC_API_KEY), `openai` (OPENAI_API_KEY), `mock` (no key, canned answer) |
| `MODE` | `readonly` | `autofix` lets low-risk playbooks run (restart, scale) |
| `DEMO_PORT` | `8088` | local port of the web console |
| `VERSION` | latest | chart version |
| `DEV` | empty | `1`: build the images of this checkout and install its chart |
| `CLUSTER` | `ventrolab-demo` | kind cluster name |

Every step is a target of its own (`cluster`, `install`, `monitoring`, `alerting`,
`workloads`, `info`), so a step can be repeated after a change.
