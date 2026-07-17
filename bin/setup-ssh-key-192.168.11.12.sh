#!/usr/bin/env bash

set -Eeuo pipefail

DEPLOY_TARGET=${DEPLOY_TARGET:-kdan@192.168.11.12}
DEPLOY_SSH_KEY=${DEPLOY_SSH_KEY:-$HOME/.ssh/new-api-192-deploy}
SSH_DIR=$(dirname "$DEPLOY_SSH_KEY")
ASKPASS_SCRIPT=""

cleanup() {
  rm -f "$ASKPASS_SCRIPT"
}
trap cleanup EXIT

mkdir -p "$SSH_DIR"
chmod 700 "$SSH_DIR"

if [[ ! -f "$DEPLOY_SSH_KEY" ]]; then
  ssh-keygen -q -t ed25519 -N '' -f "$DEPLOY_SSH_KEY" -C "new-api-deploy-192"
fi
chmod 600 "$DEPLOY_SSH_KEY"
chmod 644 "$DEPLOY_SSH_KEY.pub"

ssh_options=(
  -i "$DEPLOY_SSH_KEY"
  -o IdentitiesOnly=yes
  -o BatchMode=yes
  -o StrictHostKeyChecking=accept-new
  -o ConnectTimeout=15
)

if ssh "${ssh_options[@]}" "$DEPLOY_TARGET" true >/dev/null 2>&1; then
  printf '[ssh] Key authentication is already configured for %s\n' "$DEPLOY_TARGET"
  exit 0
fi

if [[ -z "${NEW_API_SSH_BOOTSTRAP_PASSWORD:-}" ]]; then
  read -r -s -p "Password for $DEPLOY_TARGET (used once to install the public key): " NEW_API_SSH_BOOTSTRAP_PASSWORD
  printf '\n'
fi
[[ -n "$NEW_API_SSH_BOOTSTRAP_PASSWORD" ]] || {
  printf '[error] A password is required for the one-time key installation\n' >&2
  exit 1
}

ASKPASS_SCRIPT=$(mktemp "${TMPDIR:-/tmp}/new-api-192-askpass.XXXXXX")
chmod 700 "$ASKPASS_SCRIPT"
printf '%s\n' '#!/bin/sh' 'printf "%s\\n" "$NEW_API_SSH_BOOTSTRAP_PASSWORD"' >"$ASKPASS_SCRIPT"

export NEW_API_SSH_BOOTSTRAP_PASSWORD
DISPLAY="${DISPLAY:-new-api-ssh-setup}" SSH_ASKPASS="$ASKPASS_SCRIPT" SSH_ASKPASS_REQUIRE=force \
  ssh -o PreferredAuthentications=password,keyboard-interactive -o PubkeyAuthentication=no \
    -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 "$DEPLOY_TARGET" \
    'umask 077; mkdir -p ~/.ssh; touch ~/.ssh/authorized_keys; key=$(cat); grep -qxF "$key" ~/.ssh/authorized_keys || printf "%s\\n" "$key" >> ~/.ssh/authorized_keys' \
    <"$DEPLOY_SSH_KEY.pub"

unset NEW_API_SSH_BOOTSTRAP_PASSWORD
ssh "${ssh_options[@]}" "$DEPLOY_TARGET" true
printf '[ssh] Key authentication configured for %s\n' "$DEPLOY_TARGET"
