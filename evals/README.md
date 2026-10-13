# Evals

An evaluation set that measures RCA quality end to end: real broken workloads on a
cluster, a real alert through ingest, the real controller, HolmesGPT and LLM. Run it after
every change of model, provider, prompt, skill or tool.

```
evals/run.py                          # all cases
evals/run.py bad-rollout oom-low-limit
evals/run.py --keep                   # keep namespaces and incidents to inspect them
evals/run.py --repeat 3               # answers vary between runs: report a pass rate per case
evals/compare.py results/A.json results/B.json   # side-by-side table
```

Needs `kubectl` and `helm` access to the cluster with the release (the current kube
context, namespace `ai-sre`; the kind demo of `deploy/demo` works) and PyYAML
(`.venv/bin/python`).
A full run takes about 2-3 minutes on Claude; cases are set up 4 at a time (`--parallel`).

## How a run works

1. For every case the runner creates the namespace `<case namespace>-<run id>` and applies
   the case steps, waiting after each step until the workload is really in the expected
   state (a pod reason such as `CrashLoopBackOff`, or a Job condition).
2. It posts an Alertmanager webhook to ingest with the label `aisre_eval=<run id>`.
3. The controller investigates as usual, but for evaluation incidents it sends nothing to
   chat, never remediates (the chosen playbook is still recorded) and does not use
   similar past incidents; evaluation incidents are never offered as hints either.
4. The runner waits for the diagnosis, scores it, resolves the alert and deletes the
   namespace and the incident.

Results: `results/<UTC time>-<provider>.json` (every field) and `.md` (summary table and
details of failed cases). The provider and model are read from the Helm values; LiteLLM
fallbacks are listed, because a fallback silently changes the model that answered.

## Comparing providers

Disable LiteLLM fallbacks for the run, otherwise a failing primary is silently answered by
another model. Switch the provider without editing the values file:

```
make deploy HELM_ARGS='--set llm.provider=vllm --set-json llm.fallbacks=[]'
scripts/llm-check.sh                  # the provider answers with a tool call
.venv/bin/python evals/run.py --repeat 2
make deploy                           # back to the values file
```

`results/` is not committed: keep your comparisons next to your deployment notes.

## Scoring

A case passes when all checks pass:

| Check | Rule |
|---|---|
| diagnosed | the investigation finished with a root cause |
| root_cause | every keyword group matches the root cause text (case-insensitive substring, any word of a group) |
| confidence | confidence >= `min_confidence` |
| playbook | the chosen playbook equals `expect.playbook` (`null` = none); not scored when the key is absent |

Also recorded: investigation seconds, tool calls and tool names, offered playbooks and the
reason the model gave for its choice.

## Case format

```yaml
name: crashloop-missing-config
description: what is broken and what a good answer is
namespace: billing                  # prefix; the run id is appended
steps:
  - apply: |                        # manifests, applied in the case namespace
      apiVersion: apps/v1
      kind: Deployment
      ...
    wait: {selector: app=billing-api, reasons: [CrashLoopBackOff]}   # any of the reasons
    # or: {selector: app=x, ready: true}, {job: name, condition: Failed};
    # optional: hold (seconds the condition must hold), timeout (default 300)
alert:
  alertname: KubePodCrashLooping
  labels: {container: api}          # namespace and pod (or job_name) are added
  summary: Pod {namespace}/{pod} is crash looping.
expect:
  root_cause: [[settings.yaml, config], [missing, not found]]
  playbook: null
  min_confidence: 0.7
```

Rules for good cases:
- Workloads use `busybox` and small resources; names look like real services, so the
  model is not told it is a test.
- Keyword groups name the decisive fact (file, secret, flag, limit), not generic words.
- Cases where the offered playbook is a trap (`playbook: null`) are as valuable as cases
  that need a playbook: they catch models that restart everything.

## Cases

| Case | Broken by | Expected playbook |
|---|---|---|
| crashloop-missing-config | config file not mounted, exit 1 | none |
| bad-rollout | revision 2 uses an unknown flag | rollback-failed-rollout |
| oom-low-limit | memory limit below startup need | none |
| image-pull-bad-tag | image tag does not exist | none |
| missing-secret | env var from a missing Secret | none |
| unschedulable-cpu | CPU request larger than any node | none |
| readiness-probe-failing | probe file never created | none |
| job-failed | database host does not resolve | not scored |
