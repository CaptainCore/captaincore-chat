#!/usr/bin/env bash
# One-time setup of a fresh Ubuntu 24.04 host for the relay. Run from your
# machine: deploy/setup-host.sh root@<ip> chat.example.com
# Installs Caddy (official apt repo), opens only SSH, 80 and 443, adds a 1 GB
# swap file, creates the captaincore-chat user and /opt + /etc directories.
# Safe to run again. Then: install-remote.sh, then the config (docs/deploy.md).
set -euo pipefail
host=${1:?usage: deploy/setup-host.sh <user@host> <hostname>}
name=${2:?usage: deploy/setup-host.sh <user@host> <hostname>}
dir=$(cd "$(dirname "$0")" && pwd)
sed "s/CHAT_HOST/$name/" "$dir/chat.caddy" | ssh "$host" 'cat > /tmp/captaincore-chat.caddy'
ssh "$host" 'bash -s' <<'REMOTE'
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
# A fresh droplet runs cloud-init and its first apt upgrade at boot; wait for both.
cloud-init status --wait >/dev/null 2>&1 || true
apt() { apt-get -o DPkg::Lock::Timeout=600 "$@"; }
for _ in $(seq 120); do pgrep -x apt-get >/dev/null || break; sleep 5; done
apt update -q
apt install -y -q debian-keyring debian-archive-keyring apt-transport-https curl gnupg ufw unattended-upgrades
if [ ! -f /usr/share/keyrings/caddy-stable-archive-keyring.gpg ]; then
	curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
	curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt > /etc/apt/sources.list.d/caddy-stable.list
	apt update -q
fi
apt install -y -q caddy
ufw allow OpenSSH >/dev/null
ufw allow 80/tcp >/dev/null
ufw allow 443/tcp >/dev/null
ufw --force enable >/dev/null
if [ ! -f /swapfile ]; then
	fallocate -l 1G /swapfile && chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile
	echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi
id captaincore-chat >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin captaincore-chat
install -d -m 0755 /opt/captaincore-chat
install -d -m 0750 -g captaincore-chat /etc/captaincore-chat
install -m 0644 /tmp/captaincore-chat.caddy /etc/caddy/captaincore-chat.caddy
grep -q 'import /etc/caddy/captaincore-chat.caddy' /etc/caddy/Caddyfile || {
	# Replace the stock :80 placeholder site with the relay.
	printf 'import /etc/caddy/captaincore-chat.caddy\n' > /etc/caddy/Caddyfile
}
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1
systemctl reload caddy || systemctl restart caddy
echo "ready: $(hostname) $(lsb_release -ds)"
REMOTE
