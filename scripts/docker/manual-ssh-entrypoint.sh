#!/usr/bin/env bash
set -euo pipefail

USER_NAME="${OPENCLAW_MANUAL_USER:-node}"
USER_HOME="$(getent passwd "$USER_NAME" | cut -d: -f6)"

if [[ -z "$USER_HOME" ]]; then
  echo "Unknown OPENCLAW_MANUAL_USER: $USER_NAME" >&2
  exit 1
fi

mkdir -p "$USER_HOME/.ssh"
chmod 700 "$USER_HOME/.ssh"

if [[ -n "${SSH_PUBLIC_KEY:-}" ]]; then
  printf '%s\n' "$SSH_PUBLIC_KEY" >"$USER_HOME/.ssh/authorized_keys"
  chmod 600 "$USER_HOME/.ssh/authorized_keys"
else
  echo "No SSH_PUBLIC_KEY supplied; enabling password login for local-only dev."
  echo "$USER_NAME:${OPENCLAW_MANUAL_PASSWORD:-openclaw}" | chpasswd
fi

chown -R "$USER_NAME:$USER_NAME" "$USER_HOME/.ssh"
echo "$USER_NAME ALL=(ALL) NOPASSWD:ALL" >/etc/sudoers.d/openclaw-manual
chmod 440 /etc/sudoers.d/openclaw-manual

mkdir -p "$USER_HOME/.openclaw" "$USER_HOME/.openclaw/workspace"
chown -R "$USER_NAME:$USER_NAME" "$USER_HOME/.openclaw"

/usr/sbin/sshd -e

gateway_bind="${OPENCLAW_GATEWAY_BIND:-lan}"
gateway_port="${OPENCLAW_GATEWAY_PORT:-18789}"
echo "Starting OpenClaw Gateway on bind=${gateway_bind} port=${gateway_port}"
exec sudo -u "$USER_NAME" env \
  HOME="$USER_HOME" \
  USER="$USER_NAME" \
  LOGNAME="$USER_NAME" \
  XDG_CACHE_HOME="$USER_HOME/.cache" \
  OPENCLAW_HOME="${OPENCLAW_HOME:-$USER_HOME}" \
  OPENCLAW_STATE_DIR="${OPENCLAW_STATE_DIR:-$USER_HOME/.openclaw}" \
  OPENCLAW_CONFIG_PATH="${OPENCLAW_CONFIG_PATH:-$USER_HOME/.openclaw/openclaw.json}" \
  OPENCLAW_CONFIG_DIR="${OPENCLAW_CONFIG_DIR:-$USER_HOME/.openclaw}" \
  OPENCLAW_WORKSPACE_DIR="${OPENCLAW_WORKSPACE_DIR:-$USER_HOME/.openclaw/workspace}" \
  OPENCLAW_GATEWAY_PASSWORD="${OPENCLAW_GATEWAY_PASSWORD:-}" \
  OPENCLAW_GATEWAY_BIND="$gateway_bind" \
  OPENCLAW_GATEWAY_PORT="$gateway_port" \
  bash -lc 'cd /workspace/openclaw && exec pnpm openclaw gateway --bind "$OPENCLAW_GATEWAY_BIND" --port "$OPENCLAW_GATEWAY_PORT" --allow-unconfigured'
