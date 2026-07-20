#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$ROOT_DIR/bin/deploy-common.sh"

DEPLOY_NAME=173.242.117.170
DEPLOY_SLUG=173
DEPLOY_TARGET=root@173.242.117.170
DEPLOY_INITIALIZE_ENV=true
REMOTE_DIR=/opt/newapi-proxy
COMPOSE_FILE=docker-compose.server-173.242.117.170.yml
CADDY_FILE=Caddyfile.173.242.117.170
PROXY_SERVICE=gateway
TARGET_IMAGE=new-api:release-173
ROLLBACK_IMAGE=new-api:rollback-173
BUILD_IMAGE=new-api:build-173-amd64
HEALTH_URL=https://173.242.117.170/api/status

deploy_server_main
