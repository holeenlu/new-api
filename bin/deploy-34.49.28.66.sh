#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$ROOT_DIR/bin/deploy-common.sh"

DEPLOY_NAME="ai-proxy (34.49.28.66)"
DEPLOY_SLUG=ai-proxy
DEPLOY_NODE_NAME=new-api-ai-proxy
DEPLOY_TARGET=${DEPLOY_TARGET:-root@34.49.28.66}
DEPLOY_SSH_KEY=${DEPLOY_SSH_KEY:-$HOME/.ssh/new-api-ai-proxy-deploy}
DEPLOY_INITIALIZE_ENV=true
REMOTE_DIR=/opt/newapi-proxy
COMPOSE_FILE=docker-compose.server-34.49.28.66.yml
CADDY_FILE=Caddyfile.34.49.28.66
PROXY_SERVICE=gateway
TARGET_IMAGE=new-api:release-ai-proxy
ROLLBACK_IMAGE=new-api:rollback-ai-proxy
BUILD_IMAGE=new-api:build-ai-proxy-amd64
HEALTH_URL=https://ai-proxy.kdan.cc/api/status

deploy_server_main
