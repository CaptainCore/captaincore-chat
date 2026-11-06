# CaptainCore Chat

Live chat for [CaptainCore](https://captaincore.io), built into the CaptainCore Manager. Customers chat from inside their dashboard and keep their full history. Operators work a full-screen inbox with each customer's account, sites and signals beside the thread.

This repo is the live relay: one Go binary with no database, sized for a $6 VPS. The Manager stores every conversation, runs the rules and serves the customer chat panel and the operator inbox. After each change it sends a signed event here, and the relay pushes it over websockets to the operators and the customer who should see it. The Manager also signs each user's identity. This service only verifies it, so it holds no secrets.

Status: early development. See [PLAN.md](PLAN.md) for the roadmap and [mockup/index.html](mockup/index.html) for the UI direction.

## Build

```bash
go build -o captaincore-chat .        # pure Go: CGO_ENABLED=0 works for every target
go test ./...                         # includes a PHP sodium cross-check when php is installed
```

## Run locally

```bash
./captaincore-chat dev keys --out dev.key            # stand-in for the Manager's keypair
cp config-sample.json config.json                    # paste the printed public key into public_keys
./captaincore-chat serve --config config.json        # 127.0.0.1:8490 by default

# Mint a test identity for a websocket client
./captaincore-chat dev token --key dev.key --sub 42 --role customer --name Priya
```

The `config.json` fields are:
- `listen`
- `issuer`: the Manager URL
- `audience`: this service's URL
- `public_keys`: Manager Ed25519 public keys, base64
- `allowed_origins`: defaults to the issuer

Unknown fields are rejected.

## Commands

| Command | |
|---|---|
| `serve` | Run the server. Stops gracefully on SIGINT/SIGTERM. |
| `version` | Print the version. |
| `dev keys`, `dev token` | Local keypair and test tokens. |

## Protocol

[docs/protocol.md](docs/protocol.md) covers the token format, the signed publish call (with the PHP signers the Manager uses) and the websocket frames.

## License

MIT. See [LICENSE](LICENSE).
