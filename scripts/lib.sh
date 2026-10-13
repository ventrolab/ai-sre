# Shared helpers for the scripts. Source it, do not run it.
set -euo pipefail
# Uses the current kube context (KUBECONFIG or ~/.kube/config).
NS="${NS:-ai-sre}"
RELEASE="${RELEASE:-ai-sre}"

psql_q() {
  kubectl -n "$NS" exec "$RELEASE-postgres-0" -- psql -U aisre -d aisre -tA -F ' | ' -c "$1"
}

# port_forward <service> <remote port>: sets PF_PORT, stops the forward on exit.
# Call it directly, not in $(...): a subshell would kill the forward on return.
port_forward() {
  PF_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1])')
  kubectl -n "$NS" port-forward "svc/$1" "$PF_PORT:$2" >/dev/null 2>&1 &
  PF_PIDS="${PF_PIDS:-} $!"
  trap 'kill $PF_PIDS 2>/dev/null || true' EXIT
  for _ in $(seq 1 50); do
    (echo >"/dev/tcp/127.0.0.1/$PF_PORT") 2>/dev/null && return 0
    sleep 0.2
  done
  echo "port-forward to $1 failed" >&2
  return 1
}

ingest_token() {
  kubectl -n "$NS" get secret "$RELEASE-secrets" -o jsonpath='{.data.INGEST_TOKEN}' | base64 -d
}
