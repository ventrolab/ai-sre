#!/usr/bin/env bash
# Asks HolmesGPT directly and prints each tool call with its status/error:
#   scripts/holmes-ask.sh "Describe deployment crashy in namespace demo"
# Sends Accept: application/json - Holmes refuses kubectl tools with "Accept: */*".
. "$(dirname "$0")/lib.sh"
port_forward "$RELEASE-holmes" 80; port=$PF_PORT
body=$(python3 -c 'import json,sys; print(json.dumps({"ask": sys.argv[1], "model": "default"}))' "$*")
curl -fsS -m 600 "localhost:$port/api/chat" -H 'Accept: application/json' -H 'Content-Type: application/json' -d "$body" |
python3 -c '
import json, sys
d = json.load(sys.stdin)
for t in d.get("tool_calls") or []:
    r = t.get("result") or {}
    line = "TOOL %s: %s -> %s" % (t.get("tool_name"), str(t.get("description"))[:120], r.get("status"))
    if r.get("error"):
        line += " ERROR " + str(r.get("error"))[:300]
    print(line)
print("--- analysis")
print(d.get("analysis", ""))
'
