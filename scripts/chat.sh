#!/usr/bin/env bash
# Sends a chat message to ingest as the console adapter, e.g.:
#   scripts/chat.sh /status
#   scripts/chat.sh "/reinvestigate 78c73b0a"
# Answers appear in chat-gateway logs ([answer] lines) and broadcasts also in Telegram.
. "$(dirname "$0")/lib.sh"
text="$*"
port_forward "$RELEASE-ingest" 8080; port=$PF_PORT
body=$(python3 -c 'import json,sys; print(json.dumps({"ref": {"adapter": "console", "chat_id": "cli", "user_name": "cli"}, "text": sys.argv[1]}))' "$text")
curl -fsS -X POST "localhost:$port/v1/chat" -H "Authorization: Bearer $(ingest_token)" \
  -H 'Content-Type: application/json' -d "$body"
echo
