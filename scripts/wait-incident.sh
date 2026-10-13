#!/usr/bin/env bash
# Waits until an incident leaves new/investigating (default timeout 600 s):
#   scripts/wait-incident.sh 78c73b0a [timeout]
. "$(dirname "$0")/lib.sh"
id_prefix="$1"; timeout="${2:-600}"; start=$(date +%s)
sleep 5
while :; do
  st=$(psql_q "select status from incidents where id::text like '$id_prefix%' limit 1")
  case "$st" in new|investigating) ;; *) echo "status: $st after $(( $(date +%s) - start ))s"; exit 0;; esac
  [ $(( $(date +%s) - start )) -lt "$timeout" ] || { echo "timeout, status: $st" >&2; exit 1; }
  sleep 5
done
