#!/usr/bin/env bash
set -euo pipefail

SERVER_HOST="${AGENT_BRIDGE_HOST:-agent-bridge}"
SERVER_USER="${AGENT_BRIDGE_USER:-admin}"
REMOTE="${SERVER_USER}@${SERVER_HOST}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> 1. Running verification tests..."
cd "$REPO_DIR"
go vet ./...
go test ./...

echo "==> 2. Building static linux/amd64 binary..."
mkdir -p "$REPO_DIR/bin"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "$REPO_DIR/bin/agent-bridge" ./cmd/agent-bridge

echo "==> 3. Uploading binary to EC2 (${REMOTE})..."
scp -o UserKnownHostsFile="/home/favour/.config/tailscale/ssh_known_hosts" \
    -o StrictHostKeyChecking=no \
    -o ProxyCommand="tailscale nc %h %p" \
    "$REPO_DIR/bin/agent-bridge" "${REMOTE}:/tmp/agent-bridge"

echo "==> 4. Installing binary to /usr/local/bin/agent-bridge..."
tailscale ssh "${REMOTE}" "sudo mv /tmp/agent-bridge /usr/local/bin/agent-bridge && sudo chmod 755 /usr/local/bin/agent-bridge"

echo "==> 5. Ensuring mobile bridge configuration exists on EC2..."
tailscale ssh "${REMOTE}" "bash -c '
CONFIG_FILE=\"/home/admin/.config/agent-bridge/config.yaml\"
if ! grep -q \"mobile:\" \"\$CONFIG_FILE\"; then
  cat <<EOF >> \"\$CONFIG_FILE\"

mobile:
  enabled: true
  listen_addr: \":8765\"
  auth_token: \"\"
EOF
  echo \"Added mobile configuration section to \$CONFIG_FILE\"
else
  echo \"Mobile configuration section already present in \$CONFIG_FILE\"
fi
'"

echo "==> 6. Restarting agent-bridge service..."
tailscale ssh "${REMOTE}" "sudo systemctl restart agent-bridge && sudo systemctl status agent-bridge --no-pager"

echo "==> 7. Verifying mobile listener on port 8765..."
sleep 2
tailscale ssh "${REMOTE}" "ss -tulpn | grep 8765 || journalctl -u agent-bridge -n 20 --no-pager"

echo "==> Deployment of agent-bridge with mobile bridge completed successfully!"
