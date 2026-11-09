# CaptainCore Chat

The live relay for chat in [CaptainCore](https://captaincore.io). Customers chat from inside their CaptainCore dashboard and keep their full history. Operators work a full-screen inbox with each customer's account, sites and signals beside the thread.

This repo is one small Go binary. It holds no database and no secrets, and a $6 VPS is plenty for it. Everything else lives in the [CaptainCore Manager](https://github.com/CaptainCore/captaincore-manager):

- The Manager stores every conversation, message and file, and enforces every rule.
- It serves the operator inbox, the customer's Support page and the corner chat panel.
- It signs each user's identity and every event with an Ed25519 key that never leaves it.

The relay only verifies those signatures and pushes events over websockets to the operators and the customer who should see them, along with typing indicators, draft previews and presence.

```
Browser ──REST──▶ CaptainCore Manager (stores everything, signs tokens and events)
   ▲                     │
   │ websocket           │ POST /internal/publish (signed)
   │                     ▼
   └─────────────── captaincore-chat (this repo: fan-out, typing, presence)
```

## Why it is built this way

- **Nothing to lose.** If the relay host dies, no history goes with it. Browsers fall back to refreshing from the Manager, then reconnect and refetch when the relay is back. Rebuilding a host takes a few minutes.
- **Nothing to steal.** The relay holds only public keys. Someone who takes over the host can watch live events, but can't write to a conversation or pose as anyone.
- **Small.** One static binary, no CGO, no database, one config file.

## Build

Requires Go 1.26 or newer.

```bash
go build -o captaincore-chat .        # pure Go: CGO_ENABLED=0 works for every target
go test ./...                         # includes a PHP sodium cross-check when php is on the PATH
```

## Run locally

```bash
./captaincore-chat dev keys --out dev.key            # stand-in for the Manager's keypair
cp config-sample.json config.json                    # paste the printed public key into public_keys
./captaincore-chat serve --config config.json        # listens on 127.0.0.1:8490

# Mint a test identity for a websocket client
./captaincore-chat dev token --key dev.key --sub 42 --role customer --name Priya
```

`config.json` and `*.key` are ignored by git. Keep them out of commits.

## Configuration

| Field | |
|---|---|
| `listen` | Address to bind. Defaults to `127.0.0.1:8490`; put a TLS proxy such as Caddy in front. |
| `issuer` | The Manager's URL. Tokens must carry it exactly. |
| `audience` | This relay's public URL. Tokens must carry it exactly. |
| `public_keys` | The Manager's Ed25519 public keys, base64. List two while rotating. |
| `allowed_origins` | Origins allowed to open a websocket. Defaults to the issuer. |

Unknown fields are rejected, and so is a private key pasted by mistake. The Manager prints a ready-made config with `wp captaincore chat relay --relay-url=<url>`.

## Endpoints

| Path | |
|---|---|
| `GET /v1/ws` | The websocket. The first frame authenticates with a Manager-signed token; tokens are never accepted in URLs. |
| `POST /internal/publish` | Signed events from the Manager. Answers with how many connections got the event, whether the customer was online and which operators are watching the inbox. |
| `GET /healthz` | Health check with the running version. |

## Commands

| Command | |
|---|---|
| `serve` | Run the relay. Stops gracefully on SIGINT and SIGTERM. |
| `version` | Print the version. |
| `dev keys`, `dev token` | A local keypair and test tokens. |

## Docs

- [docs/protocol.md](docs/protocol.md): the token format, the signed publish call (with the PHP signers the Manager uses) and every websocket frame.
- [docs/deploy.md](docs/deploy.md): setting up a host with Caddy, installing and updating builds, and turning chat on for customers.
- [mockup/index.html](mockup/index.html): the original UI sketch. The shipped inbox lives in the Manager.

## Status

In use, before a first tagged release. See [changelog.md](changelog.md).

## License

MIT. See [LICENSE](LICENSE).
