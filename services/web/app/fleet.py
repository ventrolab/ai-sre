"""Agent enrollment helpers shared by the admin pages and the API."""

from __future__ import annotations

import hashlib
import os
import re
import ssl
from functools import lru_cache

from .config import Config

NAME_RE = re.compile(r"^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$")
SCOPES = {
    "status:read": "Read system status, incidents and approvals (CLI aisre)",
    "agents:read": "List agents and their state",
    "agents:enroll": "Create join tokens for new agents",
    "agents:revoke": "Revoke agent credentials (tokens and certificates)",
}
MAX_TTL_HOURS = 24 * 7


def valid_name(name: str) -> bool:
    return bool(NAME_RE.match(name))


@lru_cache(maxsize=4)
def _fingerprint(path: str, mtime: float) -> str:
    with open(path) as f:
        der = ssl.PEM_cert_to_DER_cert(f.read())
    return hashlib.sha256(der).hexdigest()


def ca_fingerprint(cfg: Config) -> str:
    """Hex SHA-256 of the hub CA certificate; empty when it is not mounted."""
    if not cfg.tool_hub_ca_file or not os.path.exists(cfg.tool_hub_ca_file):
        return ""
    return _fingerprint(cfg.tool_hub_ca_file, os.path.getmtime(cfg.tool_hub_ca_file))


def install_hint(cfg: Config, token: str) -> dict[str, str]:
    """What to run on the host; hub URL, auth mode and CA pin come from the deployment.

    `ca_sha256` is empty when the agent does not need to pin the hub CA (tokens through an
    ingress with a publicly trusted certificate)."""
    hub = (cfg.tool_hub_public_url or "wss://<hub address>:<port>") + "/v1/tunnel"
    mtls = cfg.tool_hub_auth == "mtls"
    need_pin = mtls or cfg.tool_hub_public_ca != "system"
    pin = (ca_fingerprint(cfg) or "<hub CA sha256>") if need_pin else ""
    d = "/etc/ai-sre/credentials"
    if mtls:
        config = f"hubURL: {hub}\nauth: mtls\ntls:\n  ca: {d}/ca.crt\n  cert: {d}/tls.crt\n  key: {d}/tls.key"
    else:
        config = f"hubURL: {hub}\nauth: token\ntokenFile: {d}/token"
        if need_pin:
            config += f"\ntls:\n  ca: {d}/ca.crt"
    pin_arg = f" -ca-sha256 {pin}" if need_pin else ""
    return {
        "hub_url": hub,
        "auth": "mtls" if mtls else "token",
        "ca_sha256": pin,
        "config": config,
        # The service runs as user ai-sre: it must own its credentials, so enroll runs as that user.
        "command": f"install -d -o ai-sre -g ai-sre -m 700 {d}\n"
                   f"sudo -u ai-sre ai-sre-host-agent enroll -config /etc/ai-sre/host-agent.yaml \\\n"
                   f"  -token {token}{pin_arg}\n"
                   "systemctl enable --now ai-sre-host-agent",
    }
