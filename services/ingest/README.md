# Event Ingest

Receives Alertmanager webhooks, scheduler tasks and chat commands.
Deduplicates, correlates, enriches and publishes `Event` envelopes to NATS.

See ARCHITECTURE.md, sections 3.1 and 5.1. Alertmanager setup and which alerts to send:
docs/alerting.md.
