# Changelog

All notable changes to CaptainCore Chat are recorded here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

The first version: a stateless live relay for chat in the CaptainCore Manager.

### Added
- `captaincore-chat serve`: the relay. It serves a websocket at `/v1/ws`, signed events at `/internal/publish` and a health check at `/healthz`, and shuts down gracefully. It stores nothing: conversations live in the Manager, which enforces every rule.
- Signed publish from the Manager:
  - Ed25519 over a domain-separated string with a timestamp. Each signature is accepted once.
  - Operators get every event. The customer who owns the conversation gets it unless it is internal or a guest conversation.
  - The response reports how many connections got the event, how many of those were the customer's (so the Manager can hold back the email fallback while they are online) and which operators are watching the inbox (so push notifications skip them).
- Identity tokens signed by the Manager (`v1.<payload>.<signature>`). The relay holds public keys only, and several can be listed for key rotation. Tokens refresh on an open socket without reconnecting.
- Live frames:
  - Typing. An operator names the customer it is for; a customer's typing goes to operators only.
  - Draft previews. A customer's typing frame may carry the draft so far, which operators see as it is written (last 1,000 characters, its own rate limit). Operators' drafts are never relayed.
  - Presence: whether an operator is online.
  - Focus: operators report whether the inbox is on screen.
- `captaincore-chat version`, `dev keys` and `dev token`.
- Deploy scripts for a host of its own (`deploy/`): `setup-host.sh` prepares a fresh Ubuntu host with Caddy, and `install-remote.sh` builds a git ref for Linux and installs it. The host serves a small placeholder page, kept out of search engines. See `docs/deploy.md`.

### Security
- Tokens are checked for issuer, audience and expiry, are never accepted in URLs and are never logged.
- Websockets are limited to an Origin allowlist.
- Publish bodies are capped at 512 KB and unknown fields are rejected. The config rejects unknown fields and a pasted private key.
- Per-connection rate limits, and clients that can't keep up are disconnected.
- The host setup opens only SSH, 80 and 443, allows key-only SSH, adds fail2ban and unattended upgrades, and runs the relay as its own unprivileged user under systemd.
