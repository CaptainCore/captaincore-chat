# Deploying the relay

The relay runs on a small host of its own. The $6 DigitalOcean droplet (1 vCPU, 1 GB) has plenty of room for it. Caddy terminates TLS and passes the three relay paths to the binary on 127.0.0.1:8490. The host holds no secrets: the config has only the Manager's public key and URLs.

## A new host

1. Create an Ubuntu 24.04 droplet with your SSH key.
2. Point the relay's hostname at it with an A record. Caddy gets its certificate from Let's Encrypt over HTTP, so the record has to resolve before the first request. Until then Caddy keeps retrying.
3. Prepare the host:
   `deploy/setup-host.sh root@<ip> chat.example.com`
   This installs Caddy and the firewall (SSH, 80 and 443 only), a 1 GB swap file, unattended upgrades, the `captaincore-chat` user and its directories. Running it again is safe.
4. Install a build:
   `deploy/install-remote.sh root@<ip> <tag or branch>`
5. On the Manager, set the relay URL. The first run also creates the signing keypair. It prints the relay's config:
   `wp captaincore chat relay --relay-url=https://chat.example.com > config.json`
6. Copy that config to the host as `/etc/captaincore-chat/config.json` (mode 0640, group `captaincore-chat`), then run `systemctl restart captaincore-chat`.
7. Check it:
   - `curl https://chat.example.com/healthz` shows the version.
   - In the Manager, the operator inbox shows the live dot instead of refreshing on a timer.

## Updates

`deploy/install-remote.sh root@<ip> <ref>` builds the ref in a temporary worktree, replaces the binary and restarts the service. Restarting drops open websockets. Browsers reconnect by themselves and refetch, so no event is lost: the Manager keeps every message and the relay only forwards them.

## Turning chat on for customers

Customer access is the Manager's `captaincore_chat_mode` option. Operators always get the inbox.

- `off` (the default): customers keep Intercom.
- `preview`: chat for customers whose user meta `captaincore_chat_preview` is 1. Everyone else keeps Intercom.
  `wp user meta update <user> captaincore_chat_preview 1`
- `on`: chat for everyone, and Intercom is no longer loaded.

Set it with `wp captaincore chat relay --mode=<mode>`. Going back to `off` brings Intercom back on the next page load, and the conversations stay in the Manager.
