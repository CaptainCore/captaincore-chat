#!/usr/bin/env bash
# One-time setup of a fresh Ubuntu 24.04 host for the relay. Run from your
# machine: deploy/setup-host.sh root@<ip> chat.example.com
# Installs every pending update and Caddy (official apt repo), opens only
# SSH, 80 and 443, hardens SSH (keys only, no forwarding) with fail2ban,
# sets kernel network defaults, turns on nightly security updates with
# automatic reboots, adds a 1 GB swap file, and creates the captaincore-chat
# user and its directories.
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
apt install -y -q caddy fail2ban
apt full-upgrade -y -q -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold
apt autoremove -y -q

# SSH: keys only, root by key (the deploy scripts use it), no forwarding.
# Drop-ins are read in order and the first value wins, so 10- beats the
# cloud image's own 50-/60- files.
cat > /etc/ssh/sshd_config.d/10-hardening.conf <<'CONF'
PermitRootLogin prohibit-password
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
X11Forwarding no
AllowAgentForwarding no
AllowTcpForwarding no
MaxAuthTries 3
LoginGraceTime 30
ClientAliveInterval 300
ClientAliveCountMax 2
CONF
sshd -t
systemctl reload ssh

# Ban addresses that keep failing SSH logins.
cat > /etc/fail2ban/jail.d/sshd.local <<'CONF'
[sshd]
enabled = true
backend = systemd
maxretry = 5
findtime = 10m
bantime = 1h
CONF
systemctl enable --quiet fail2ban
systemctl restart fail2ban

# Kernel network defaults for a host that routes nothing.
cat > /etc/sysctl.d/90-hardening.conf <<'CONF'
net.ipv4.conf.all.rp_filter = 1
net.ipv4.conf.default.rp_filter = 1
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.default.accept_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv6.conf.default.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.default.send_redirects = 0
net.ipv4.conf.all.accept_source_route = 0
net.ipv6.conf.all.accept_source_route = 0
net.ipv4.conf.all.log_martians = 1
net.ipv4.icmp_echo_ignore_broadcasts = 1
net.ipv4.tcp_syncookies = 1
kernel.kptr_restrict = 2
kernel.dmesg_restrict = 1
CONF
sysctl -q --system

# Desktop and storage helpers a droplet never uses.
for unit in ModemManager.service udisks2.service multipathd.service multipathd.socket; do
	systemctl disable --now --quiet "$unit" 2>/dev/null || true
done

# Security updates install daily; when one needs a reboot it happens at
# 09:00 UTC (4 or 5 AM Eastern). Browsers reconnect to the relay by themselves.
cat > /etc/apt/apt.conf.d/52captaincore-chat <<'CONF'
Unattended-Upgrade::Remove-Unused-Kernel-Packages "true";
Unattended-Upgrade::Remove-Unused-Dependencies "true";
Unattended-Upgrade::Automatic-Reboot "true";
Unattended-Upgrade::Automatic-Reboot-Time "09:00";
CONF
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
install -d -m 0755 /srv/captaincore-chat/www
install -d -m 0750 -g captaincore-chat /etc/captaincore-chat
install -m 0644 /tmp/captaincore-chat.caddy /etc/caddy/captaincore-chat.caddy
grep -q 'import /etc/caddy/captaincore-chat.caddy' /etc/caddy/Caddyfile || {
	# Replace the stock :80 placeholder site with the relay.
	printf 'import /etc/caddy/captaincore-chat.caddy\n' > /etc/caddy/Caddyfile
}
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1
systemctl reload caddy || systemctl restart caddy
echo "ready: $(hostname) $(lsb_release -ds)$( [ -f /var/run/reboot-required ] && echo ", reboot required")"
REMOTE
