#!/usr/bin/env bash
# Issues a client certificate for an agent, signed by the Tool Hub CA.
# The CommonName is the target name the agent appears under.
#   scripts/issue-agent-cert.sh <name> <outdir> [days]
# Writes <outdir>/{ca.crt,tls.crt,tls.key}. The CA key never leaves this machine.
. "$(dirname "$0")/lib.sh"
name="$1"; out="$2"; days="${3:-825}"
[[ "$name" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] || { echo "invalid name: $name" >&2; exit 1; }
mkdir -p "$out" && chmod 700 "$out"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
kubectl -n "$NS" get secret "$RELEASE-tool-hub-ca" -o jsonpath='{.data.ca\.crt}' | base64 -d > "$out/ca.crt"
kubectl -n "$NS" get secret "$RELEASE-tool-hub-ca" -o jsonpath='{.data.ca\.key}' | base64 -d > "$tmp/ca.key"
openssl ecparam -name prime256v1 -genkey -noout -out "$out/tls.key" 2>/dev/null
openssl req -new -key "$out/tls.key" -subj "/CN=$name" -out "$tmp/req.csr"
printf 'extendedKeyUsage=clientAuth\nkeyUsage=digitalSignature\n' > "$tmp/ext"
openssl x509 -req -in "$tmp/req.csr" -CA "$out/ca.crt" -CAkey "$tmp/ca.key" -CAcreateserial \
  -days "$days" -extfile "$tmp/ext" -out "$out/tls.crt" 2>/dev/null
chmod 600 "$out/tls.key"
echo "issued $out/tls.crt for $name, valid $days days"
