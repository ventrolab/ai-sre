---
name: certificate-expiry
description: TLS certificate expiring or expired, cert-manager Certificate not ready, x509 errors
---

## Workflow
1. Find the certificate: cert-manager `Certificate` resources, the TLS Secret
   referenced by the Ingress, or probe metrics (`probe_ssl_earliest_cert_expiry`).
2. cert-manager: read `Certificate`, `CertificateRequest`, `Order`, `Challenge`
   status; HTTP-01 failures usually mean the challenge path is not reachable,
   DNS-01 failures mean DNS credentials or propagation.
3. Check the issuer (`ClusterIssuer`) is Ready.

## Remediation
Fix the issuer or challenge reachability, then let cert-manager renew. Manual
certificate replacement needs a human.
