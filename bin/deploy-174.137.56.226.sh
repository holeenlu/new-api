#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$ROOT_DIR/bin/deploy-common.sh"

DEPLOY_NAME=174.137.56.226
DEPLOY_SLUG=174
DEPLOY_TARGET=root@174.137.56.226
REMOTE_DIR=/opt/newapi-proxy
COMPOSE_FILE=docker-compose.server-174.137.56.226.yml
CADDY_FILE=Caddyfile.174.137.56.226
PROXY_SERVICE=caddy
TARGET_IMAGE=new-api:release-174
ROLLBACK_IMAGE=new-api:rollback-174
BUILD_IMAGE=new-api:build-174-amd64
HEALTH_URL=https://nextcode.buildtoconnect.com/api/status

deploy_server_main
