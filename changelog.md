# Changelog

## [Unreleased]

### Added
- `captaincore-chat serve`: a stateless live relay for CaptainCore Manager chat. It serves a websocket at `/v1/ws` and a health check at `/healthz`, and shuts down gracefully. Conversations are stored in the Manager, which enforces every rule; this service stores nothing.
- `POST /internal/publish`: the Manager sends each change as a signed event.
  - The signature is Ed25519 over a domain-separated string with a timestamp, and each signature is accepted once.
  - Operators get every event. The owning customer gets the event unless it is internal or belongs to a guest conversation.
  - The response says how many connections got the event and how many of those were the customer's.
- Identity: Ed25519 tokens signed by the Manager (`v1.<payload>.<signature>`). The service holds public keys only, and several keys can be listed for rotation. Tests sign tokens and publish bodies with PHP sodium, as the Manager does, and verify them in Go.
- Live frames:
  - Typing: an operator names the customer it is for. A customer's typing goes to operators only, tagged with the sender's id.
  - Presence: whether an operator is online.
  - Token refresh without reconnecting.
- Hardening:
  - An Origin allowlist for websockets.
  - Tokens are never accepted in URLs or written to logs.
  - Publish bodies are capped at 512 KB, and unknown fields are rejected.
  - Clients that can't keep up are disconnected.
- `captaincore-chat version`, `dev keys` and `dev token`.
