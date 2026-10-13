# aisre

Terminal client of AI SRE: system status, incidents with their root cause, approvals and
agents, read from the web console API (`/api/v1`).

## Install

Release assets `aisre-<version>-<linux|darwin>-<amd64|arm64>.tar.gz` (with `SHA256SUMS-binaries`),
or the OCI artifact:

```
oras pull ghcr.io/<owner>/ai-sre/aisre-bin:<version>
tar xzf aisre-<version>-linux-amd64.tar.gz
install -m 755 aisre-<version>-linux-amd64/aisre /usr/local/bin/aisre
```

Every CI run on `main` also uploads the four binaries as the workflow artifact
`aisre-<commit>` (kept 14 days). From source: `make cli` -> `dist/aisre`.

## Sign in

An admin creates an API token in the console: Administration -> API tokens, scope
`status:read` (and `agents:read` for `aisre agents`).

```
aisre login --url https://ai-sre.example.com     # asks for the token, checks it, saves both
```

The config lives in `$XDG_CONFIG_HOME/aisre/config.yaml` (default `~/.config/aisre/`,
mode 0600). `AISRE_URL` and `AISRE_TOKEN`, or `--url` and `--token`, override it.

## Commands

| Command | Shows |
|---|---|
| `aisre status` | version, remediation mode and limits, LLM provider, open incidents by status, MTTD/MTTR, pending approvals, component health, top alerts; exits 2 if a component is unhealthy |
| `aisre incidents [-a] [-s STATUS] [-q TEXT] [-n N]` | open incidents (`-a` all, `-s` one status, `-q` search) |
| `aisre incident ID` | one incident: alerts, root cause, analysis, approvals, timeline; ID may be the 8-character prefix from the list |
| `aisre approvals` | pending approvals and recent decisions |
| `aisre agents` | agents of the Tool Hub: state, auth, version, tools, last seen, labels |

`-o json` prints the API answer for scripts, e.g.
`aisre incidents -o json | jq -r '.incidents[].title'`.

The CLI only reads. Approvals stay in the chat and the console, where they are audited
per user.
