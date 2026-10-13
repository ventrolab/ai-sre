#!/usr/bin/env bash
# Lists incidents, or shows one incident with its timeline:
#   scripts/incidents.sh            # latest 20
#   scripts/incidents.sh 78c73b0a   # id prefix: details + events
. "$(dirname "$0")/lib.sh"
if [ $# -eq 0 ]; then
  psql_q "select left(id::text,8), status, coalesce(round(confidence::numeric,2)::text,'-'), coalesce(playbook,'-'), title, to_char(updated_at,'MM-DD HH24:MI') from incidents order by updated_at desc limit 20"
  exit 0
fi
id=$(psql_q "select id from incidents where id::text like '$1%' limit 1")
[ -n "$id" ] || { echo "no incident $1" >&2; exit 1; }
psql_q "select 'status: '||status, 'confidence: '||coalesce(confidence::text,'-'), 'playbook: '||coalesce(playbook,'-'), 'decision: '||coalesce(decision->>'decision','-')||' '||coalesce(decision->>'reason',''), 'root cause: '||coalesce(root_cause,'-') from incidents where id='$id'" | tr '|' '\n' | sed 's/^ //'
echo "--- timeline"
psql_q "select to_char(ts,'HH24:MI:SS'), type, left(data::text,160) from incident_events where incident_id='$id' order by ts"
