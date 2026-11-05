# Changelog

## [Unreleased]

### Added
- `captaincore-chat serve`: REST API under `/v1`, websocket at `/v1/ws`, health check at `/healthz`, and a graceful shutdown.
- Identity: Ed25519 tokens signed by the Manager (`v1.<payload>.<signature>`). The service holds public keys only, and several keys can be listed for rotation. A test signs with PHP sodium, as the Manager will, and verifies in Go.
- Storage in a single SQLite file with versioned migrations: conversations, messages and read marks.
- Conversation rules:
  - A customer reply starts the waiting clock and reopens a closed or snoozed thread. An operator reply clears the clock.
  - Notes and internal events are never shown to customers.
  - Snoozes wake on their own.
- Access rules:
  - Customers see only their own conversations, and someone else's returns 404.
  - Site and account links must come from the token.
  - Customers can resolve or reopen a conversation. Everything else is for operators.
- Live events: new conversations and messages, status changes, typing, read receipts, and whether an operator is online. Customers receive only their own public events.
- Hardening:
  - An Origin allowlist for CORS and websockets.
  - Tokens are never accepted in URLs or written to logs.
  - Message bodies are plain text with control characters stripped, capped at 10,000 characters.
  - Rate limits of 20 messages and 5 new conversations per user per minute.
  - Request bodies are capped at 64 KB and unknown JSON fields are rejected.
- `captaincore-chat migrate`, `version`, `dev keys` and `dev token`.
