#!/usr/bin/env bash
# Checks an LLM Gateway model with a Holmes-like request (tools, temperature,
# max_tokens=64000) and fallbacks disabled, so the real backend error is visible:
#   scripts/llm-check.sh            # model "default"
#   scripts/llm-check.sh vllm
. "$(dirname "$0")/lib.sh"
model="${1:-default}"
kubectl -n "$NS" exec "deploy/$RELEASE-litellm" -- python -c "
import json, os, urllib.error, urllib.request
body = {'model': '$model', 'disable_fallbacks': True, 'max_tokens': 64000, 'temperature': 0.00000001,
        'messages': [{'role': 'user', 'content': 'List pods in namespace demo'}],
        'tools': [{'type': 'function', 'function': {'name': 'kubectl_get_pods', 'description': 'List pods',
                   'parameters': {'type': 'object', 'properties': {'namespace': {'type': 'string'}}, 'required': ['namespace']}}}]}
req = urllib.request.Request('http://localhost:4000/v1/chat/completions', data=json.dumps(body).encode(),
                             headers={'Authorization': 'Bearer ' + os.environ['LITELLM_MASTER_KEY'], 'Content-Type': 'application/json'})
try:
    d = json.load(urllib.request.urlopen(req, timeout=180))
    m = d['choices'][0]['message']
    print('$model OK', d['choices'][0]['finish_reason'], [t['function']['arguments'] for t in m.get('tool_calls') or []])
except urllib.error.HTTPError as e:
    print('$model HTTP', e.code, e.read()[:500].decode())
"
