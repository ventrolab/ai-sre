# Contributing to Ventrolab AI SRE

Thank you for helping. Bug reports, playbooks, skills, adapters and fixes are all welcome.

## Before you start

- Bugs and feature ideas: open an issue first for anything larger than a small fix, so the
  design can be agreed before you write code.
- Security issues: do not open a public issue, see [SECURITY.md](SECURITY.md).
- Design background: [ARCHITECTURE.md](ARCHITECTURE.md) and [docs/decisions.md](docs/decisions.md).
  Developer setup and tests: [docs/development.md](docs/development.md).

## Developer Certificate of Origin

Every commit must be signed off: by adding the line

```
Signed-off-by: Your Name <you@example.com>
```

you certify the [Developer Certificate of Origin 1.1](https://developercertificate.org/):
that you wrote the change or otherwise have the right to submit it under the project's
license (Apache-2.0). `git commit -s` adds the line. The DCO check on pull requests rejects
commits without it; fix them with `git rebase --signoff main` and force-push.

There is no CLA.

## Pull requests

- One topic per pull request, with a description of what changes and why.
- `make test` passes (Python and Go; Go runs in Docker). Chart changes: `make lint-chart`.
  Behaviour that an alert investigation depends on: a case in `evals/` helps.
- Code, comments, logs and docs are in English. No emoji.
- Keep the product generic: no company names, hostnames, chat ids or credentials in code,
  examples or tests.
- New playbooks need a risk class, a precheck and a verification query; new skills need a
  clear trigger. See the existing ones in `playbooks/default` and `skills`.

## Code of conduct

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md).
