# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub:
**Security -> Report a vulnerability** on this repository (private vulnerability reporting).
Do not open public issues or pull requests for security problems.

Include the affected component and version, the impact, and steps to reproduce. We
acknowledge reports within 3 business days and keep you informed until a fix is released.
We credit reporters in the advisory unless you prefer otherwise.

## Supported versions

Security fixes go into the latest minor release. Older versions are not patched; upgrade to
the latest release.

## Scope

In scope: the services, agents, CLI, Helm charts and container images published from this
repository. Especially relevant:
- the agent tunnel and its authentication (agent tokens, mTLS, enrollment);
- Action Guard decisions: anything that lets an action run without the required approval or
  above the configured risk;
- secret redaction in tool output sent to the LLM;
- authentication and roles in the web console and the API.

Out of scope: vulnerabilities in third-party components (report them upstream; tell us if
our use of them makes it exploitable), and findings that need an already compromised
cluster administrator account.

## Verifying releases

Release images are signed with cosign (keyless, GitHub OIDC) and carry SBOM and provenance
attestations, see [docs/releasing.md](docs/releasing.md).
