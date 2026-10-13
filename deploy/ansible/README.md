# Ansible: host agents on standalone hosts

Role `ai_sre_host_agent` installs the host-agent on hosts that are not Kubernetes nodes and
enrolls it unattended:

1. Service user `ai-sre` (group `adm` for logs), binary, `/etc/ai-sre/host-agent.yaml`.
2. If the host has no credential yet (or `ai_sre_host_agent_reenroll: true`): the
   controller asks the console API for a one-time join token
   (`POST /api/v1/agents/enrollments`, API token with scope `agents:enroll`), and the
   host runs `ai-sre-host-agent enroll`, which exchanges it for an agent token
   (`ai_sre_agent_auth: token`, the default) or, with `mtls`, pins the hub CA, creates the
   key on the host and gets a certificate signed by the hub. The API token stays on the
   controller; the join token reaches the host through the environment and is not logged.
3. Systemd unit, enabled and started.

```
# group_vars/ai_sre_hosts.yml
ai_sre_console_url: https://ai-sre.example.com
ai_sre_api_token: !vault |
  ...
# The tunnel ingress of the hub (toolHub.tunnel.ingress, public certificate). For the hub's
# own TLS port (wss://host:8443/v1/tunnel) also set ai_sre_hub_ca_pinned: true.
ai_sre_hub_url: wss://agent.example.com/v1/tunnel
ai_sre_host_agent_binary_src: ../../dist/host-agent
ai_sre_host_agent_labels: {env: prod, role: database}
ai_sre_host_agent_log_files: [/var/log/syslog, /var/log/postgresql/*.log]
```

```
ansible-playbook -i inventory.ini deploy/ansible/host-agents.yml --ask-vault-pass
```

Re-running is safe: hosts with a credential are not enrolled again. Renewal or a
reinstalled host: `-e ai_sre_host_agent_reenroll=true` (replaces the registry entry of the
same name). All variables: `roles/ai_sre_host_agent/defaults/main.yml`.

Removing a host: Administration -> Agents -> Revoke (or `POST /api/v1/agents/<name>/revoke`),
then stop the service.
