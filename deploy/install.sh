#!/bin/sh
# Build Autobot and install or upgrade it on the triage host.
# Usage: deploy/install.sh [ssh-host]   (default: triage)
set -eu
host=${1:-triage}
cd "$(dirname "$0")/.."

# A release passes the full tests first. AUTOBOT_SKIP_TESTS=1 skips them, for an emergency only.
if [ "${AUTOBOT_SKIP_TESTS:-}" != "1" ]; then
	scripts/test.sh full
fi

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/autobot-linux ./cmd/autobot
scp -q /tmp/autobot-linux "$host":autobot.new
scp -q deploy/autobot.service "$host":autobot.service.new
rm -f /tmp/autobot-linux

ssh "$host" 'set -eu
command -v git >/dev/null || echo "warning: git is not installed on this host; agents will read code through GitHub (sudo apt-get install git)"
id autobot >/dev/null 2>&1 || sudo useradd --system --home-dir /var/lib/autobot --no-create-home --shell /usr/sbin/nologin autobot
if ! sudo test -f /etc/autobot/secret.key; then
	sudo install -d -m 0700 -o root -g root /etc/autobot
	head -c 32 /dev/urandom | od -An -tx1 | tr -d " \n" | sudo tee /etc/autobot/secret.key >/dev/null
	sudo chmod 0600 /etc/autobot/secret.key
	echo "created /etc/autobot/secret.key: back it up separately from the database"
fi
sudo install -m 0755 autobot.new /usr/local/bin/autobot
sudo install -m 0644 autobot.service.new /etc/systemd/system/autobot.service
rm -f autobot.new autobot.service.new
sudo systemctl daemon-reload
sudo systemctl enable autobot.service >/dev/null
sudo systemctl restart autobot.service
sleep 1
systemctl is-active autobot.service
sudo journalctl -u autobot.service -n 5 --no-pager'
