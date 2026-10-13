# Playbooks

Remediation playbook catalog. Loaded at runtime from `PLAYBOOKS_PATH`; editing it does not require a rebuild.

- `default/` - catalog shipped with the Helm chart. Every entry can be disabled or overridden through Helm values or a user ConfigMap.
- Format, validation and reload rules: ARCHITECTURE.md, section 3.9.

A playbook selects one primitive (implemented in code) and defines parameters, risk class, precheck, verify, rollback and limits. `SRE_MODE`, `AUTOFIX_MAX_RISK` and policies are always applied on top.
