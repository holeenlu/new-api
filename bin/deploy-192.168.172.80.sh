#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$ROOT_DIR/bin/deploy-common.sh"

DEPLOY_NAME="CC00-AI (192.168.172.80)"
DEPLOY_SLUG=cc00-ai
DEPLOY_TARGET=kdanmobile@192.168.172.80
DEPLOY_SSH_KEY=${DEPLOY_SSH_KEY:-$HOME/.ssh/new-api-cc00-ai-deploy}
DEPLOY_INITIALIZE_ENV=true
REMOTE_DIR=/home/kdanmobile/newapi-proxy
COMPOSE_FILE=docker-compose.server-192.168.172.80.yml
CADDY_FILE=Caddyfile.192.168.172.80
PROXY_SERVICE=gateway
TARGET_IMAGE=new-api:release-cc00-ai
ROLLBACK_IMAGE=new-api:rollback-cc00-ai
BUILD_IMAGE=new-api:build-cc00-ai-amd64
HEALTH_URL=http://192.168.172.80:3000/api/status

deploy_server_main
