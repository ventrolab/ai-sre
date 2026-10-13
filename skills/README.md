# Skills (knowledge base)

Troubleshooting guides for HolmesGPT. Each directory holds a `SKILL.md` with
YAML frontmatter (`name`, `description`) and a markdown body. Holmes matches a
skill by its `description` and loads it with the `fetch_skill` tool when an
alert or question fits.

The chart ships these skills in the ConfigMap `<release>-skills`; an init
container in the Holmes pod lays them out as `<name>/SKILL.md` under
`/etc/ai-sre/skills`. Add or override skills with `knowledge.extraSkills` in the
chart values (name -> content). Holmes reads skills at startup: after changing
them, restart the Holmes Deployment (`make deploy` does it).

Writing a skill:
- `description` decides when the skill is used: list the symptoms and alert names.
- Workflow steps should be procedural and evidence-driven: what to read, in
  which order, and how to interpret it.
- Say when a restart does not help, and which playbook from the catalog fits.
