# Releasing

GitHub Actions in `.github/workflows`:

| Workflow | Trigger | Does |
|---|---|---|
| `ci.yml` | push to `main`, pull requests | Python tests, Go vet/test (`-race`, both modules), gofmt, helm lint/render, `aisre` binaries as workflow artifact; then images: pull requests build amd64 without pushing, `main` pushes `edge` and `sha-<commit>` (amd64+arm64) |
| `release.yml` | tag `vX.Y.Z` or `vX.Y.Z-rc.N` | everything below, then a GitHub Release with generated notes (pre-release for `-` versions) |
| `images.yml` | called by the two above | one job per image, buildx, GHCR, SBOM + provenance, cosign keyless signature |

```
git tag v0.2.0 && git push origin v0.2.0
```

## Release artifacts

| Artifact | Where |
|---|---|
| Images (ingest, controller, chat-gateway, web, action-guard, tool-hub, k8s-agent) | `ghcr.io/<owner>/ai-sre/<name>:<version>`, `:<major>.<minor>`, `:latest`; linux/amd64 and linux/arm64 |
| Helm charts `ai-sre`, `k8s-agent` | `oci://ghcr.io/<owner>/ai-sre/charts/<chart>` (version = release); `.tgz` on the release |
| Agent binaries | release assets `ai-sre-{host,k8s}-agent-<version>-linux-{amd64,arm64}.tar.gz` (the host-agent archive also holds the systemd unit, example config and README), `SHA256SUMS-*` |
| Agent binaries as OCI artifacts | `ghcr.io/<owner>/ai-sre/{host,k8s}-agent-bin:<version>` (`oras pull ...`) |
| CLI `aisre` | release assets `aisre-<version>-{linux,darwin}-{amd64,arm64}.tar.gz`, OCI `ghcr.io/<owner>/ai-sre/aisre-bin:<version>`; every `main` CI run uploads the binaries as workflow artifact `aisre-<commit>` |

Charts default to the images of their own version (`image.tag` empty = chart
`appVersion`), so `helm install oci://.../ai-sre --version X` needs no image settings.

Verify a signature:
```
cosign verify ghcr.io/<owner>/ai-sre/web:<version> \
  --certificate-identity-regexp 'https://github.com/<owner>/ai-sre/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Public access

- GHCR packages (images, charts, OCI binaries) have their own visibility. A new package
  is created private; make it public once per package in GitHub: profile -> Packages ->
  package -> Package settings -> Change visibility. There is no API for user-owned
  packages.
- Release assets on GitHub follow the repository visibility: while the repository is
  private, public users get the binaries from the OCI artifacts above.

## Local builds

`make images` (local registry, current commit tag) and `make -C deploy/demo demo DEV=1`
(kind) use the same Dockerfiles; they cross-compile Go with `--platform=$BUILDPLATFORM`, so
the same files serve buildx multi-arch builds in CI.
