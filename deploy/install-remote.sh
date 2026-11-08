#!/usr/bin/env bash
# Builds a ref of this repo for Linux and installs it on a host prepared by
# setup-host.sh. The build comes from a temporary worktree of the ref, never
# the working tree. Restarting drops open websockets; browsers reconnect on
# their own and refetch, so nothing is lost.
# Usage: deploy/install-remote.sh <user@host> <ref>    e.g. deploy/install-remote.sh root@<ip> master
set -euo pipefail
host=${1:?usage: deploy/install-remote.sh <user@host> <ref>}
ref=${2:?usage: deploy/install-remote.sh <user@host> <ref>}
repo=$(cd "$(dirname "$0")/.." && pwd)
commit=$(git -C "$repo" rev-parse --verify "$ref^{commit}")
version=$(git -C "$repo" describe --tags --always "$commit")
tmp=$(mktemp -d)
trap 'git -C "$repo" worktree remove --force "$tmp/src" >/dev/null 2>&1 || true; rm -rf "$tmp"' EXIT
git -C "$repo" worktree add --detach --quiet "$tmp/src" "$commit"
(cd "$tmp/src" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
	-ldflags "-s -w -X github.com/CaptainCore/captaincore-chat/version.Version=$version" -o "$tmp/captaincore-chat" .)
scp -q "$tmp/captaincore-chat" "$host:/opt/captaincore-chat/captaincore-chat.new"
scp -q "$tmp/src/deploy/captaincore-chat.service" "$host:/etc/systemd/system/captaincore-chat.service"
ssh "$host" 'chmod 0755 /opt/captaincore-chat/captaincore-chat.new \
	&& mv /opt/captaincore-chat/captaincore-chat.new /opt/captaincore-chat/captaincore-chat \
	&& systemctl daemon-reload && systemctl enable --quiet captaincore-chat \
	&& if [ -f /etc/captaincore-chat/config.json ]; then systemctl restart captaincore-chat && sleep 2 \
	   && systemctl is-active captaincore-chat && curl -s http://127.0.0.1:8490/healthz && echo; \
	   else echo "binary installed; add /etc/captaincore-chat/config.json, then systemctl restart captaincore-chat"; fi'
echo "installed $version ($(git -C "$repo" rev-parse --short "$commit")) on $host"
