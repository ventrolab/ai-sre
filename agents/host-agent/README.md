# host-agent

Read-only diagnostics of a standalone host (not a Kubernetes node), served to the Tool
Hub over an outbound WebSocket tunnel, authenticated with an agent token (or mutual TLS).
Tools: `host_info`,
`host_processes`, `host_service_status`, `host_journal`, `host_log_file`, `host_command`.

Install:

1. `make host-agent` -> `dist/host-agent` (static binary); copy to `/usr/local/bin/ai-sre-host-agent`.
2. `useradd -r -s /usr/sbin/nologin ai-sre`, install `packaging/ai-sre-host-agent.service`.
3. Config from `packaging/host-agent.example.yaml` to `/etc/ai-sre/host-agent.yaml`
   (`hubURL` = `toolHub.publicUrl` + `/v1/tunnel`).
4. Credential with a join token: an admin creates it in the web console (Administration ->
   Agents -> Add an agent) or automation calls `POST /api/v1/agents/enrollments` with an
   API token (scope `agents:enroll`). The answer contains the token, the config lines and
   the commands:
   ```
   install -d -o ai-sre -g ai-sre -m 700 /etc/ai-sre/credentials
   sudo -u ai-sre ai-sre-host-agent enroll -config /etc/ai-sre/host-agent.yaml \
     -token <join token>
   systemctl enable --now ai-sre-host-agent
   ```
   `enroll` exchanges the join token (it works once) for an agent token, written to
   `tokenFile` (0600). The hub is verified with the system roots (tunnel ingress); for the
   hub's own TLS port add `-ca-sha256 <hub CA sha256>` and `tls.ca`, the CA is pinned.
   With `auth: mtls` it generates the key on the host and gets a certificate signed by the
   hub instead. The join token can also come from the environment (`AISRE_JOIN_TOKEN`),
   which keeps it out of the process list.
5. Renewal or a new host: create a new token with "re-enroll" (`replace: true`) and run
   `enroll` again; earlier agent tokens of the name stop working.

Manual alternative without the registry: `scripts/issue-agent-cert.sh <host-name> <dir>`
and copy `ca.crt`, `tls.crt`, `tls.key` to `/etc/ai-sre/tls/`.

Revocation: Administration -> Agents -> Revoke (or `POST /api/v1/agents/{name}/revoke`).
The hub drops the session within 30 s and refuses the agent's tokens and certificate.

Safety: commands run without a shell; unit names, priorities and paths are validated;
log files must match the allowlist after symlink resolution; nothing writes to the host.
